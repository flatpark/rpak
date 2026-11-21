package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/flatpark/rpak/internal/args"
	"github.com/flatpark/rpak/internal/doctor"
	"github.com/flatpark/rpak/internal/extradata"
	fr "github.com/flatpark/rpak/internal/forceruntime"
	"github.com/flatpark/rpak/internal/installation"
	"github.com/flatpark/rpak/internal/metadata"
	"github.com/flatpark/rpak/internal/ostree"
	"github.com/flatpark/rpak/internal/plan"
	"github.com/flatpark/rpak/internal/prefetch"
	"github.com/flatpark/rpak/internal/summary"
	"github.com/flatpark/rpak/internal/units"
)

// rpak install/update --force-runtime and --extra-data, and the records
// they leave, see packages forceruntime and extradata. Without the options
// and without records nothing here runs: transaction only checks that the
// records file is missing, and the command is run as before, without any
// extra flatpak command.

// transaction runs an update or install command, argv[i] being the
// command.
func transaction(version, kind string, argv []string, i int) int {
	parsed := args.Parse(kind, argv[i+1:])
	if !parsed.ForceRuntime && len(parsed.ExtraData) == 0 {
		user, err := prefetch.DefaultUserBaseDir()
		if err != nil || !fr.HasRecords(user) || parsed.SkipReason == "--help" ||
			(kind == "install" && parsed.SkipReason != "") {
			return runFlatpak(version, kind, argv, i)
		}
	}
	f, code, ok := newForceFlow(version, kind, argv, i, parsed, os.Stderr)
	if !ok {
		return code
	}
	defer f.close()
	if kind == "install" {
		return f.install()
	}
	return f.update()
}

// forceFlow is an install or update that forces runtimes, or keeps them
// forced.
type forceFlow struct {
	ctx           context.Context
	version, kind string
	globals       []string // global options before the command
	// args are the command's arguments as given, options those of them
	// that are options, without --force-runtime.
	args    []string
	options []string
	parsed  args.Parsed
	stderr  io.Writer

	flatpak string
	query   plan.Runner
	// user is the user installation; insts are all installations, where
	// runtimes may be installed.
	user  string
	insts []string

	records []fr.Record
	// before are the records as loaded: what changed is what a dry run
	// shows, and what is announced (announceChanged).
	before []fr.Record
	// extras are the extra-data records, extrasBefore as loaded.
	extras, extrasBefore []fr.ExtraRecord
	// specs are the --extra-data values given.
	specs []extradata.Spec
	// back are apps whose last patch went away by itself (upstream
	// replaced the source): installed from their remote again after the
	// update.
	back []item
	// fetched are the directories of the files downloaded for
	// --extra-data; ran reports that a flatpak transaction ran, which
	// used them (and removes them, see runFlatpak).
	fetched []string
	ran     bool
	// warned are the apps whose experimental line has been printed.
	warned   map[string]bool
	dryRun   bool
	branches map[string][]string
	unlock   func()
}

// forceQueryTimeout bounds each flatpak command run for forcing; exports
// and summary updates of the local repository take a few seconds.
const forceQueryTimeout = 10 * time.Minute

func newForceFlow(version, kind string, argv []string, i int, parsed args.Parsed, stderr io.Writer) (*forceFlow, int, bool) {
	f := &forceFlow{
		ctx: context.Background(), version: version, kind: kind,
		globals: append([]string(nil), argv[:i]...),
		args:    argv[i+1:],
		options: args.Options(kind, argv[i+1:]),
		parsed:  parsed, stderr: stderr,
		flatpak:  flatpakPath(),
		dryRun:   dryRunEnv(),
		branches: map[string][]string{},
		unlock:   func() {},
	}
	f.query = plan.ExecRunner{Path: f.flatpak, Timeout: forceQueryTimeout}
	if parsed.ForceRuntime {
		if err := fr.ValidTarget(parsed.ForceTarget); err != nil {
			fmt.Fprintf(stderr, "rpak: %v\n", err)
			return nil, 2, false
		}
		if parsed.SkipReason != "" {
			fmt.Fprintf(stderr, "rpak: --force-runtime cannot be used with %s\n", parsed.SkipReason)
			return nil, 2, false
		}
	}
	specs, err := parseSpecs(parsed.ExtraData)
	if err != nil {
		fmt.Fprintf(stderr, "rpak: %v\n", err)
		return nil, 2, false
	}
	f.specs = specs
	if len(specs) > 0 && parsed.SkipReason != "" {
		fmt.Fprintf(stderr, "rpak: %s cannot be used with %s\n", extradata.Option, parsed.SkipReason)
		return nil, 2, false
	}
	user, err := prefetch.DefaultUserBaseDir()
	if err != nil {
		fmt.Fprintf(stderr, "rpak: cannot find the flatpak user directory: %v\n", err)
		return nil, 1, false
	}
	f.user = user
	for _, inst := range (installation.Env{}).All() {
		f.insts = append(f.insts, inst.Path)
	}
	if !f.dryRun {
		unlock, err := lockForce(user)
		if err != nil {
			fmt.Fprintf(stderr, "rpak: %v\n", err)
			return nil, 1, false
		}
		f.unlock = unlock
	}
	f.setRecords(loadRecords(user, stderr, !f.dryRun))
	return f, 0, true
}

// parseSpecs reads the --extra-data values; two for the same NAME are an
// error (two that pick the same source are found when it is picked).
func parseSpecs(values []string) ([]extradata.Spec, error) {
	var out []extradata.Spec
	seen := map[string]bool{}
	for _, v := range values {
		s, err := extradata.ParseSpec(v)
		if err != nil {
			return nil, err
		}
		if s.Name != "" && seen[s.Name] {
			return nil, fmt.Errorf("%s=%s=...: %s is given twice", extradata.Option, s.Name, s.Name)
		}
		seen[s.Name] = true
		out = append(out, s)
	}
	return out, nil
}

// given names the patching options given, for messages.
func (f *forceFlow) given() string {
	var opts []string
	if f.parsed.ForceRuntime {
		opts = append(opts, args.ForceRuntimeOption)
	}
	if len(f.specs) > 0 {
		opts = append(opts, extradata.Option)
	}
	return strings.Join(opts, " and ")
}

// dryRunEnv reports RPAK_DRY_RUN: show what would be done, do nothing.
func dryRunEnv() bool {
	return os.Getenv("RPAK_DRY_RUN") != "" && os.Getenv("RPAK_DRY_RUN") != "0"
}

// setRecords sets the records as loaded, and a copy of them in before.
func (f *forceFlow) setRecords(records []fr.Record, extras []fr.ExtraRecord) {
	f.records = records
	f.before = append([]fr.Record(nil), records...)
	f.extras = extras
	f.extrasBefore = append([]fr.ExtraRecord(nil), extras...)
}

// hasPatches reports whether any app is patched.
func (f *forceFlow) hasPatches() bool { return len(f.records) > 0 || len(f.extras) > 0 }

// close ends the flow: the files downloaded for --extra-data go if no
// flatpak transaction got to use them (the next try downloads them again,
// not knowing their checksums before), and the lock is released.
func (f *forceFlow) close() {
	if !f.ran && len(f.fetched) > 0 {
		if err := prefetch.RemoveDirs(f.user, f.fetched); err != nil {
			f.warn("removing the downloaded extra-data: %v", err)
		}
	}
	f.unlock()
}

// lockForce keeps two rpak runs from forcing at the same time: flock() on
// <user installation>/rpak/.lock, waited for.
func lockForce(user string) (func(), error) {
	if err := os.MkdirAll(fr.Dir(user), 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(fr.Dir(user), ".lock"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); errors.Is(err, unix.EWOULDBLOCK) {
		fmt.Fprintf(os.Stderr, "rpak: waiting for another rpak forcing runtimes\n")
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX)
		if err != nil {
			file.Close()
			return nil, err
		}
	} else if err != nil {
		file.Close()
		return nil, err
	}
	return func() { file.Close() }, nil
}

// loadRecords reads the records of both kinds. A file that cannot be read
// is moved aside (with move) and taken as no records: patching must never
// keep rpak from updating.
func loadRecords(user string, stderr io.Writer, move bool) ([]fr.Record, []fr.ExtraRecord) {
	records, err := fr.Load(user)
	var extras []fr.ExtraRecord
	if err == nil {
		if extras, err = fr.LoadExtra(user); err == nil {
			return records, extras
		}
	}
	path := fr.RecordsPath(user)
	if move {
		if rerr := os.Rename(path, path+".broken"); rerr == nil {
			fmt.Fprintf(stderr, "rpak: warning: cannot read %s (%v); moved it to %s.broken, no patch is kept\n", path, err, path)
			return nil, nil
		}
	}
	fmt.Fprintf(stderr, "rpak: warning: cannot read %s (%v); no patch is kept\n", path, err)
	return nil, nil
}

func (f *forceFlow) warn(format string, a ...any) {
	fmt.Fprintf(f.stderr, "rpak: warning: "+format+"\n", a...)
}

func (f *forceFlow) note(format string, a ...any) {
	fmt.Fprintf(f.stderr, "rpak: "+format+"\n", a...)
}

func (f *forceFlow) fail(code int, format string, a ...any) int {
	fmt.Fprintf(f.stderr, "rpak: "+format+"\n", a...)
	return code
}

// experimental is the line printed whenever an app is made to run on
// another runtime; undo is the --reset that undoes it.
func experimental(id string, from, to fr.Pref, undo string) string {
	return fmt.Sprintf("rpak: experimental: %s was built for %s, now runs on %s; it may break. Undo: rpak patch %s %s", id, from, to, undo, id)
}

// announce prints the experimental line of an app, once per run, and not
// in a dry run.
func (f *forceFlow) announce(ref string, from, to fr.Pref) {
	if f.dryRun || f.warned[ref] {
		return
	}
	if f.warned == nil {
		f.warned = map[string]bool{}
	}
	f.warned[ref] = true
	// Plain --reset would undo the app's replaced extra-data too.
	undo := "--reset"
	if _, ok := fr.FindExtra(f.extras, ref); ok {
		undo += "=" + fr.Kind
	}
	fmt.Fprintln(f.stderr, experimental(idOf(ref), from, to, undo))
}

// announceChanged prints the experimental line of every app whose record
// is new or has a new target and that is forced, even if its deployment
// needed no change (it was on the target already). A record that stayed as
// it was says nothing; deploying an app anew (repatch, an export) does.
func (f *forceFlow) announceChanged() {
	for _, r := range f.records {
		old, had := fr.Find(f.before, r.Ref)
		if had && targetText(old.Target) == targetText(r.Target) {
			continue
		}
		from, err1 := fr.ParsePref(r.Original)
		to, err2 := fr.ParsePref(r.Runtime)
		if err1 != nil || err2 != nil || from == to {
			continue
		}
		f.announce(r.Ref, from, to)
	}
	for _, r := range f.extras {
		old, _ := fr.FindExtra(f.extrasBefore, r.Ref)
		if sameFiles(old.Sources, r.Sources) {
			continue
		}
		for _, line := range experimentalExtra(r, old) {
			fmt.Fprintln(f.stderr, line)
		}
	}
}

// sameFiles reports whether two sets of replacements download the same
// files from the same places.
func sameFiles(a, b []extradata.Replacement) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].File != b[i].File {
			return false
		}
	}
	return true
}

// experimentalExtra are the lines printed when an app's replaced
// extra-data are new or changed: what it downloads now, instead of what,
// and, last, how to undo it. Not printed again while the record stays.
func experimentalExtra(r, old fr.ExtraRecord) []string {
	var out []string
	for _, rep := range r.Sources {
		if o, ok := findReplacement(old.Sources, rep.Name); ok && o.File == rep.File {
			continue
		}
		out = append(out, fmt.Sprintf("rpak: experimental: %s downloads its extra-data %s from %s (%s, sha256 %s) instead of %s; that file is your choice, its packagers did not pick it.",
			idOf(r.Ref), rep.Name, rep.URL, units.Size(rep.Size), rep.SHA256, rep.Base.URL))
	}
	if len(out) == 0 {
		// Only sources went.
		return nil
	}
	return append(out, "rpak: Undo: rpak patch --reset="+fr.ExtraKind+" "+idOf(r.Ref))
}

func findReplacement(reps []extradata.Replacement, name string) (extradata.Replacement, bool) {
	for _, r := range reps {
		if r.Name == name {
			return r, true
		}
	}
	return extradata.Replacement{}, false
}

// save writes the records and announces the new ones. A dry run writes
// nothing and shows what would change instead.
func (f *forceFlow) save() {
	if f.dryRun {
		f.showChanges()
		return
	}
	f.announceChanged()
	if err := fr.Save(f.user, f.records); err != nil {
		f.warn("cannot save %s: %v", fr.RecordsPath(f.user), err)
	}
	if err := fr.SaveExtra(f.user, f.extras); err != nil {
		f.warn("cannot save %s: %v", fr.RecordsPath(f.user), err)
	}
	f.before = append([]fr.Record(nil), f.records...)
	f.extrasBefore = append([]fr.ExtraRecord(nil), f.extras...)
}

// dry prints what a dry run would do.
func (f *forceFlow) dry(format string, a ...any) {
	fmt.Fprintf(f.stderr, "rpak: dry run, would "+format+"\n", a...)
}

// dryCmd prints a flatpak command a dry run would run.
// An argument with a newline (build-export's --body) is in bash's $'...'
// form, so that the command stays on one line.
func (f *forceFlow) dryCmd(argv []string) {
	quoted := make([]string, 0, len(argv)+1)
	for _, a := range append([]string{f.flatpak}, argv...) {
		if strings.Contains(a, "\n") {
			r := strings.NewReplacer(`\`, `\\`, `'`, `\'`, "\n", `\n`)
			quoted = append(quoted, "$'"+r.Replace(a)+"'")
		} else {
			quoted = append(quoted, shellQuote([]string{a}))
		}
	}
	f.dry("run: %s", strings.Join(quoted, " "))
}

// showChanges is the dry run's save: the records that would be added,
// changed or removed. The runtime of a record not applied yet (its
// Runtime still the original one) is what its target resolves to.
func (f *forceFlow) showChanges() {
	changed := false
	for _, r := range f.records {
		if old, had := fr.Find(f.before, r.Ref); had && old == r {
			continue
		}
		changed = true
		runtime := r.Runtime
		if orig, err := fr.ParsePref(r.Original); err == nil && runtime == r.Original {
			if res, err := f.resolve(orig, r.Target); err == nil {
				runtime = res.Runtime.String()
			}
		}
		verb := "record"
		if _, had := fr.Find(f.before, r.Ref); had {
			verb = "update the record of"
		}
		f.dry("%s %s in %s: %s, target %s, method %s, %s -> %s", verb, idOf(r.Ref), fr.RecordsPath(f.user),
			fr.Kind, targetText(r.Target), r.Method, r.Original, runtime)
	}
	for _, r := range f.before {
		if _, ok := fr.Find(f.records, r.Ref); !ok {
			changed = true
			f.dry("forget the %s patch of %s in %s", fr.Kind, idOf(r.Ref), fr.RecordsPath(f.user))
		}
	}
	for _, r := range f.extras {
		old, had := fr.FindExtra(f.extrasBefore, r.Ref)
		if had && reflect.DeepEqual(old, r) {
			continue
		}
		changed = true
		verb := "record"
		if had {
			verb = "update the record of"
		}
		for _, rep := range r.Sources {
			f.dry("%s %s in %s: %s %s, %s instead of %s", verb, idOf(r.Ref), fr.RecordsPath(f.user), fr.ExtraKind, rep.Name, rep.URL, rep.Base.URL)
		}
	}
	for _, r := range f.extrasBefore {
		if _, ok := fr.FindExtra(f.extras, r.Ref); !ok {
			changed = true
			f.dry("forget the %s patch of %s in %s", fr.ExtraKind, idOf(r.Ref), fr.RecordsPath(f.user))
		}
	}
	if !changed {
		f.dry("leave %s as it is", fr.RecordsPath(f.user))
	}
}

// dropStale forgets, without a word, the records of apps that are no
// longer installed in the user installation (uninstalled with flatpak),
// and removes the local repository if nothing needs it any more.
func (f *forceFlow) dropStale() {
	var kept []fr.Record
	for _, r := range f.records {
		if _, ok, _ := fr.Active(f.user, r.Ref); ok {
			kept = append(kept, r)
		}
	}
	var keptExtra []fr.ExtraRecord
	for _, r := range f.extras {
		if _, ok, _ := fr.Active(f.user, r.Ref); ok {
			keptExtra = append(keptExtra, r)
		}
	}
	if len(kept) == len(f.records) && len(keptExtra) == len(f.extras) {
		return
	}
	var gone []string
	for _, r := range f.records {
		if _, ok := fr.Find(kept, r.Ref); !ok && r.Method == fr.MethodReexport {
			gone = append(gone, r.Ref)
		}
	}
	for _, r := range f.extras {
		if _, ok := fr.FindExtra(keptExtra, r.Ref); !ok && !slices.Contains(gone, r.Ref) {
			gone = append(gone, r.Ref)
		}
	}
	f.records, f.extras = kept, keptExtra
	if !f.dryRun {
		// A dry run shows the forgotten records with the rest.
		f.save()
	}
	if len(gone) > 0 {
		f.unexport(gone)
	}
}

// unexport removes refs from the local repository and, once no record
// reexports, the repository and its remote, unless something installed
// still comes from it.
func (f *forceFlow) unexport(refs []string) {
	_, hasRemote := fr.FindRemote(f.user, fr.RemoteName)
	_, statErr := os.Stat(fr.RepoPath(f.user))
	// The refs are about to stop coming from rpak-local (reset reinstalls
	// them first, the others are uninstalled): a dry run, which has not
	// reinstalled them, sees them there still.
	var users []string
	for _, ref := range fr.LocalUsers(f.user) {
		if !slices.Contains(refs, ref) {
			users = append(users, ref)
		}
	}
	decision := fr.DecideLocalCleanup(f.records, f.extras, users, hasRemote || statErr == nil)
	if f.dryRun {
		if statErr == nil {
			f.dry("remove %s from the local repository %s", strings.Join(refs, ", "), fr.RepoPath(f.user))
		}
		switch decision {
		case fr.RemoveLocal:
			if hasRemote {
				f.dryCmd(fr.RemoteDeleteArgv())
			}
			f.dry("remove %s: no app would come from it any more", fr.RepoPath(f.user))
		case fr.KeepLocalInUse:
			f.dry("keep the remote %s: %s still installed from it", fr.RemoteName, strings.Join(users, ", "))
			f.dryCmd(fr.PublishArgv(f.user))
		default:
			if statErr == nil {
				f.dryCmd(fr.PublishArgv(f.user))
			}
		}
		return
	}
	if err := fr.Unexport(f.user, refs); err != nil {
		f.warn("removing %s from the local repository: %v", strings.Join(refs, ", "), err)
	}
	switch decision {
	case fr.RemoveLocal:
		if err := fr.RemoveLocalRepo(f.ctx, f.query, f.user); err != nil {
			f.warn("removing the remote %s: %v", fr.RemoteName, err)
		} else {
			f.note("removed the remote %s and its repository: no app comes from it any more", fr.RemoteName)
		}
	case fr.KeepLocalInUse:
		f.warn("keeping the remote %s: %s still installed from it", fr.RemoteName, strings.Join(users, ", "))
		f.publish()
	default:
		if statErr == nil {
			f.publish()
		}
	}
}

func (f *forceFlow) publish() {
	if err := fr.Publish(f.ctx, f.query, f.user); err != nil {
		f.warn("updating the local repository: %v", err)
	}
}

// userRemotes are the remotes of the user installation.
func (f *forceFlow) userRemotes() map[string]fr.Remote {
	remotes, _ := fr.Remotes(f.user)
	out := map[string]fr.Remote{}
	for _, r := range remotes {
		out[r.Name] = r
	}
	return out
}

// branchesOf lists the branches of a runtime that exist: installed in
// any installation, or in the summary of a remote of the user
// installation. Remotes that cannot be read are warned about and left out.
func (f *forceFlow) branchesOf(id, arch string) []string {
	key := id + "/" + arch
	if b, ok := f.branches[key]; ok {
		return b
	}
	out := fr.InstalledBranches(f.insts, id, arch)
	client := prefetch.NewHTTPClient()
	remotes, _ := fr.Remotes(f.user)
	for _, rem := range remotes {
		if rem.Disabled || rem.Name == fr.RemoteName || strings.HasPrefix(rem.URL, "oci+") {
			continue
		}
		ctx, cancel := context.WithTimeout(f.ctx, time.Minute)
		data, err := doctor.FetchSummary(ctx, client, f.version, rem.URL, arch, rem.Subset)
		cancel()
		var refs []summary.Ref
		if err == nil {
			refs, err = summary.Find(data, "runtime/"+id+"/"+arch+"/")
		}
		if err != nil {
			f.warn("looking for branches of %s in %s: %v", id, rem.Name, err)
			continue
		}
		for _, r := range refs {
			out = append(out, r.Name[strings.LastIndexByte(r.Name, '/')+1:])
		}
	}
	f.branches[key] = out
	return out
}

// resolve works out the runtime for an app built for orig.
func (f *forceFlow) resolve(orig fr.Pref, target string) (fr.Resolution, error) {
	var branches []string
	if target == "" || target == fr.Latest {
		branches = f.branchesOf(orig.ID, orig.Arch)
	}
	return fr.Resolve(orig, target, branches)
}

// remoteRef is what remote-info says of a ref of the user installation.
type remoteRef struct {
	ref, commit, metadata string
}

func (f *forceFlow) remoteInfo(remote, ref string) (remoteRef, error) {
	argv := []string{"remote-info", "--user", "--show-ref", "--show-commit", "--show-metadata"}
	if f.parsed.Arch != "" {
		argv = append(argv, "--arch="+f.parsed.Arch)
	}
	out, err := f.query.Output(f.ctx, append(argv, remote, ref)...)
	if err != nil {
		return remoteRef{}, err
	}
	// "REF COMMIT", then the metadata (flatpak_builtin_remote_info()).
	first, rest, _ := strings.Cut(string(out), "\n")
	fields := strings.Fields(first)
	if len(fields) != 2 || !ostree.ValidChecksum(fields[1]) || len(strings.Split(fields[0], "/")) != 4 {
		return remoteRef{}, fmt.Errorf("unexpected remote-info output %q", first)
	}
	return remoteRef{ref: fields[0], commit: fields[1], metadata: rest}, nil
}

// cmdArgv is a flatpak command line: the global options, the command,
// then the rest.
func (f *forceFlow) cmdArgv(command string, rest ...string) []string {
	return append(append(append([]string(nil), f.globals...), command), rest...)
}

// runTransaction runs flatpak install/update with prefetching.
func (f *forceFlow) runTransaction(kind string, argv []string) int {
	f.ran = true
	return runFlatpak(f.version, kind, argv, len(f.globals))
}

// runLocal runs a flatpak install/update from rpak-local; a dry run only
// shows it: what it would install is not exported, so it cannot be
// planned.
func (f *forceFlow) runLocal(kind string, argv []string) int {
	if f.dryRun {
		f.dryCmd(argv)
		return 0
	}
	return f.runTransaction(kind, argv)
}

// normal runs the command as given (without --force-runtime): nothing is
// forced.
func (f *forceFlow) normal() int {
	return f.runTransaction(f.kind, f.cmdArgv(f.kind, args.StripForceRuntime(f.args)...))
}

// item is an app being patched.
type item struct {
	arg string // the argument that named it
	remoteRef
	// upstream is the remote the app comes from.
	upstream string
	// res is the runtime it runs on; runtimePatch reports a force-runtime
	// patch, given or recorded (res may still be the original runtime,
	// when upstream got there).
	res          fr.Resolution
	runtimePatch bool
	target       string
	method       string
	// extra are its extra-data replacements after this run, nil for none.
	extra []extradata.Replacement
	// installed: the app is installed in the user installation;
	// fromLocal: from rpak-local.
	installed, fromLocal bool
}

// job is the reexport of the item.
func (it item) job() fr.Job {
	return fr.Job{Ref: it.ref, Upstream: it.commit, Runtime: it.res, ExtraData: it.extra}
}

func (f *forceFlow) userOnlyOption() bool {
	return f.parsed.ExplicitInstallation && len(f.parsed.Installations) == 1 && f.parsed.Installations[0] == args.User
}

// newItem works out what an app is forced to, from upstream's commit.
// ok is false (with a note) when upstream is already on the target.
func (f *forceFlow) newItem(arg, upstream string, info remoteRef, target string) (item, bool, error) {
	it := item{arg: arg, remoteRef: info, upstream: upstream, target: target}
	orig, err := fr.RuntimeOf(info.metadata)
	if err != nil {
		return it, false, err
	}
	if it.res, err = f.resolve(orig, target); err != nil {
		return it, false, err
	}
	it.method = fr.MethodPatch
	if fr.HasExtraData(info.metadata) {
		it.method = fr.MethodReexport
	}
	it.runtimePatch = true
	if d, ok, _ := fr.Active(f.user, info.ref); ok {
		it.installed, it.fromLocal = true, d.Origin == fr.RemoteName
	}
	f.attachExtra(&it)
	return it, it.res.Forced, nil
}

// plainItem is an app reexported without forcing its runtime: for its
// extra-data.
func (f *forceFlow) plainItem(arg, upstream string, info remoteRef) (item, error) {
	it := item{arg: arg, remoteRef: info, upstream: upstream, method: fr.MethodReexport}
	orig, err := fr.RuntimeOf(info.metadata)
	if err != nil {
		return it, err
	}
	it.res = fr.Resolution{Original: orig, Runtime: orig}
	if d, ok, _ := fr.Active(f.user, info.ref); ok {
		it.installed, it.fromLocal = true, d.Origin == fr.RemoteName
	}
	f.attachExtra(&it)
	return it, nil
}

// attachExtra gives an item the extra-data replacements recorded for its
// app that still apply to the upstream commit it is made of (see
// extradata.Carry); those upstream changed are dropped, and said so.
func (f *forceFlow) attachExtra(it *item) {
	r, ok := fr.FindExtra(f.extras, it.ref)
	if !ok {
		return
	}
	kept, dropped := extradata.Carry(r.Sources, it.metadata, it.ref)
	for _, d := range dropped {
		f.note("%s: upstream updated %s, %s patch dropped", idOf(it.ref), d.Name, fr.ExtraKind)
	}
	it.extra = kept
	if len(kept) > 0 {
		it.method = fr.MethodReexport
	}
}

// recordItem records what an item made of its app, once that is
// installed: its forced runtime, its extra-data replacements (none removes
// the record).
func (f *forceFlow) recordItem(it item) {
	if it.runtimePatch {
		if it.method == fr.MethodReexport {
			f.records = fr.Set(f.records, reexportRecord(it))
		} else {
			f.records = fr.Set(f.records, fr.Record{
				Ref: it.ref, Target: it.target, Remote: it.upstream, Method: fr.MethodPatch,
				Original: it.res.Original.String(), Runtime: it.res.Original.String(),
			})
		}
	}
	f.extras = fr.SetExtra(f.extras, fr.ExtraRecord{Ref: it.ref, Remote: it.upstream, Upstream: it.commit, Sources: it.extra})
}

// announceItem prints the experimental line of an item whose runtime is
// forced, as its deployment is made anew. Its extra-data are announced
// when their record changes (announceChanged).
func (f *forceFlow) announceItem(it item) {
	if it.res.Forced {
		f.announce(it.ref, it.res.Original, it.res.Runtime)
	}
}

// upstreamOf is the remote an app from rpak-local was reexported from, and
// the upstream commit, as its records have them.
func (f *forceFlow) upstreamOf(ref string) (remote, commit string, ok bool) {
	if r, ok := fr.Find(f.records, ref); ok {
		return r.Remote, r.Upstream, true
	}
	if r, ok := fr.FindExtra(f.extras, ref); ok {
		return r.Remote, r.Upstream, true
	}
	return "", "", false
}

// extraItem is an app given --extra-data: reexported from upstream's
// commit info with the sources the specs choose replaced (downloaded
// here, see fetchExtra), its other recorded replacements kept, and its
// runtime forced if --force-runtime is given or recorded. code is the
// exit code of an error: 2 for what the user has to say otherwise.
func (f *forceFlow) extraItem(arg, upstream string, info remoteRef) (item, int, error) {
	hasExtra := metadata.HasExtraData(info.metadata)
	if len(f.specs) > 0 && !hasExtra {
		return item{}, 2, fmt.Errorf("%s has no extra-data: %s replaces the download of an extra-data source", idOf(info.ref), extradata.Option)
	}
	target, forced := f.parsed.ForceTarget, f.parsed.ForceRuntime
	r, recorded := fr.Find(f.records, info.ref)
	if !forced && recorded {
		target, forced = r.Target, true
	}
	var it item
	var err error
	if forced {
		var ok bool
		if it, ok, err = f.newItem(arg, upstream, info, target); err != nil {
			return it, 1, err
		}
		if !ok {
			f.note("%s: upstream is already on %s, not forcing", idOf(info.ref), it.res.Original)
			it.runtimePatch = recorded
		}
	} else if it, err = f.plainItem(arg, upstream, info); err != nil {
		return it, 1, err
	}
	if hasExtra {
		it.method = fr.MethodReexport
	}
	reps, code, err := f.replacements(it)
	if err != nil {
		return it, code, err
	}
	it.extra = extradata.Merge(it.extra, reps)
	return it, 0, nil
}

// replacements works out the sources f.specs replace in the app of it
// (extradata.Choose) and downloads the new files, which gives their
// checksums and sizes; a dry run only says what it would download.
func (f *forceFlow) replacements(it item) ([]extradata.Replacement, int, error) {
	sources, _ := metadata.Parse(it.metadata, it.ref)
	chosen := make([]metadata.Source, len(f.specs))
	for i, spec := range f.specs {
		src, auto, err := extradata.Choose(sources, spec)
		if err != nil {
			return nil, 2, fmt.Errorf("%s: %w", idOf(it.ref), err)
		}
		for j := range i {
			if chosen[j].Name == src.Name {
				return nil, 2, fmt.Errorf("%s: %s and %s both replace %s", idOf(it.ref), f.specs[j].URL, spec.URL, src.Name)
			}
		}
		chosen[i] = src
		if auto {
			f.note("Using source %s (%s → %s)", src.Name, src.URI, spec.URL)
		}
	}
	var out []extradata.Replacement
	for i, spec := range f.specs {
		src := chosen[i]
		if f.dryRun {
			f.dry("download %s as %s of %s, instead of %s", spec.URL, src.Name, idOf(it.ref), src.URI)
			out = append(out, extradata.Replacement{Name: src.Name, File: extradata.File{URL: spec.URL, SHA256: "<sha256 of " + src.Name + ">"}, Base: extradata.FileOf(src)})
			continue
		}
		got, err := f.fetchExtra(spec.URL, src.Name)
		if err != nil {
			return nil, 1, fmt.Errorf("downloading %s: %w", spec.URL, err)
		}
		out = append(out, extradata.Replacement{Name: src.Name, File: extradata.FileOf(got), Base: extradata.FileOf(src)})
	}
	return out, 0, nil
}

// fetchExtra downloads a replacement into the extra-data directory, where
// flatpak finds it when it installs the reexported commit: the transaction
// that follows reuses it (prefetch) instead of downloading it again.
func (f *forceFlow) fetchExtra(uri, name string) (metadata.Source, error) {
	release, _, err := prefetch.LockShared(f.user, true)
	if err != nil {
		return metadata.Source{}, err
	}
	defer release()
	cfg := prefetch.NewConfig(f.user, f.version)
	rep := newReporter(f.stderr, detectTerm(os.Stderr), nil)
	var src metadata.Source
	err = nil
	rep.watch(cfg, func(cfg prefetch.Config) (prefetch.Summary, bool) {
		cfg.OnStart = func(s metadata.Source) {
			rep.expect(s)
			rep.started(s)
		}
		cfg.OnDone = func(s metadata.Source, r prefetch.Result) {
			if r.Status != prefetch.Downloaded {
				// No "flatpak will download it": the caller fails.
				r.Status = prefetch.Canceled
			}
			rep.done(s, r)
		}
		var dir string
		src, dir, err = prefetch.New(cfg).Fetch(f.ctx, uri, name)
		if err != nil {
			// The error is the caller's to report.
			return prefetch.Summary{}, true
		}
		if dir != "" {
			f.fetched = append(f.fetched, dir)
		}
		return prefetch.Summary{Downloaded: 1, Dirs: []string{dir}}, false
	})
	return src, err
}

// install is rpak install with --force-runtime or --extra-data, or with
// records.
func (f *forceFlow) install() int {
	forced := f.parsed.ForceRuntime || len(f.specs) > 0
	f.dropStale()
	if !forced && !f.hasPatches() {
		return f.normal()
	}
	user := f.userRemotes()
	sysRemotes, _ := fr.Remotes(installation.Env{}.SystemDefault().Path)
	inSystem := func(name string) bool {
		for _, r := range sysRemotes {
			if r.Name == name {
				return true
			}
		}
		return false
	}
	remote, refs := args.SplitInstallRemote(f.parsed.Positionals, func(name string) bool {
		_, ok := user[name]
		return ok || inSystem(name)
	})
	// Without REMOTE, as flatpak install does, the remote is the one that
	// has the refs: rpak looks only at the user installation's, and says
	// --user itself.
	picked := false
	if remote == "" && (!f.parsed.ExplicitInstallation || f.userOnlyOption()) && (forced || f.recorded(refs)) {
		found, err := f.findRemote(refs)
		if err != nil {
			if forced {
				return f.fail(2, "%v", err)
			}
			return f.normal()
		}
		f.note("%s: found in the remote %s", strings.Join(refs, ", "), found)
		remote, picked = found, true
	}
	_, userHas := user[remote]
	// Only the user installation is supported. Without --force-runtime
	// anything else is simply not forced.
	addUser := false
	switch {
	case remote == "":
		if forced {
			return f.fail(2, "%s only works for the user installation (--user)", f.given())
		}
		return f.normal()
	case f.parsed.ExplicitInstallation && !f.userOnlyOption():
		if forced {
			return f.fail(2, "%s only works for the user installation (--user)", f.given())
		}
		return f.normal()
	case !userHas:
		if forced {
			return f.fail(2, "%s only works for the user installation, which has no remote %s", f.given(), remote)
		}
		return f.normal()
	case !f.parsed.ExplicitInstallation && inSystem(remote) && !picked:
		if forced {
			return f.fail(2, "%s is a remote of both the user and the system installation; add --user", remote)
		}
		return f.normal()
	case !f.parsed.ExplicitInstallation:
		// The remote is only the user installation's, so flatpak would
		// install there; say so, the second flatpak command must agree.
		addUser = true
	}
	if remote == fr.RemoteName {
		if forced {
			return f.fail(2, "%s is rpak's own remote; install from the app's remote", remote)
		}
		return f.normal()
	}
	if len(f.specs) > 0 && len(refs) != 1 {
		return f.fail(2, "%s replaces the extra-data of one app at a time", extradata.Option)
	}
	if !forced && !f.recorded(refs) {
		// Only refs with a record are patched: look no further if none of
		// the arguments can be one of them.
		return f.normal()
	}

	var items, plain []item
	for _, arg := range refs {
		if strings.HasPrefix(arg, "-") {
			return f.fail(2, "%s: not a ref", arg)
		}
		info, err := f.remoteInfo(remote, arg)
		if err != nil {
			if forced {
				return f.fail(1, "%s: %v", arg, err)
			}
			plain = append(plain, item{arg: arg})
			continue
		}
		if !strings.HasPrefix(info.ref, "app/") {
			if forced {
				return f.fail(2, "%s is not an app; only apps can be patched", info.ref)
			}
			plain = append(plain, item{arg: arg})
			continue
		}
		_, hasExtra := fr.FindExtra(f.extras, info.ref)
		if len(f.specs) > 0 || (!f.parsed.ForceRuntime && hasExtra) {
			// Replacing extra-data, or keeping them replaced.
			it, code, err := f.extraItem(arg, remote, info)
			if err != nil {
				return f.fail(code, "%v", err)
			}
			if len(it.extra) == 0 && !it.res.Forced {
				plain = append(plain, item{arg: arg})
				continue
			}
			items = append(items, it)
			continue
		}
		target := f.parsed.ForceTarget
		if !f.parsed.ForceRuntime {
			r, ok := fr.Find(f.records, info.ref)
			if !ok {
				plain = append(plain, item{arg: arg})
				continue
			}
			target = r.Target
		}
		it, ok, err := f.newItem(arg, remote, info, target)
		if err != nil {
			return f.fail(1, "%s: %v", info.ref, err)
		}
		if !ok && len(it.extra) == 0 {
			f.note("%s: upstream is already on %s, not forcing", idOf(info.ref), it.res.Original)
			plain = append(plain, item{arg: arg})
			continue
		}
		items = append(items, it)
	}
	return f.apply(remote, addUser, items, plain, nil)
}

// recorded reports whether one of the install arguments can name an app
// with a patch record.
func (f *forceFlow) recorded(refs []string) bool {
	for _, ref := range refs {
		for _, r := range f.records {
			if plan.Matches(r.Ref, ref) {
				return true
			}
		}
		for _, r := range f.extras {
			if plan.Matches(r.Ref, ref) {
				return true
			}
		}
	}
	return false
}

// findRemote is the remote of the user installation that has all of refs,
// for an install without REMOTE: there must be exactly one (rpak's own
// and disabled remotes do not count).
func (f *forceFlow) findRemote(refs []string) (string, error) {
	remotes, _ := fr.Remotes(f.user)
	var found, names []string
	for _, rem := range remotes {
		if rem.Disabled || rem.Name == fr.RemoteName {
			continue
		}
		names = append(names, rem.Name)
		all := len(refs) > 0
		for _, ref := range refs {
			if strings.HasPrefix(ref, "-") || args.LocalLocation(ref) {
				all = false
				break
			}
			if _, err := f.remoteInfo(rem.Name, ref); err != nil {
				all = false
				break
			}
		}
		if all {
			found = append(found, rem.Name)
		}
	}
	what := strings.Join(refs, " ")
	switch {
	case len(found) == 1:
		return found[0], nil
	case len(found) > 1:
		return "", fmt.Errorf("%s is in more than one remote of the user installation (%s); say which: rpak install %s REMOTE %s", what, strings.Join(found, ", "), f.given(), what)
	case len(names) == 0:
		return "", fmt.Errorf("the user installation has no remote to install %s from", what)
	default:
		return "", fmt.Errorf("no remote of the user installation (%s) has %s; rpak install %s REMOTE %s", strings.Join(names, ", "), what, f.given(), what)
	}
}

// update is rpak update with --force-runtime REF... or --extra-data REF,
// or with records.
func (f *forceFlow) update() int {
	forced := f.parsed.ForceRuntime || len(f.specs) > 0
	userInvolved := !f.parsed.ExplicitInstallation
	for _, inst := range f.parsed.Installations {
		userInvolved = userInvolved || inst == args.User
	}
	if forced {
		if f.parsed.ExplicitInstallation && !f.userOnlyOption() {
			return f.fail(2, "%s only works for the user installation (--user)", f.given())
		}
		if len(f.parsed.Positionals) == 0 {
			return f.fail(2, "update %s needs the apps to patch: rpak update %s REF...", f.given(), f.given())
		}
	}
	if !userInvolved {
		return f.normal()
	}
	f.dropStale()
	if !forced && !f.hasPatches() {
		return f.normal()
	}

	var items []item
	moving := map[string]bool{} // arguments of apps reinstalled from rpak-local
	// pending are apps given --extra-data that come from rpak-local: their
	// export cannot wait for the records, which they are not in yet.
	var pending []item
	if forced {
		installed := fr.Installed(f.user, "app")
		var all []string
		for _, arg := range f.parsed.Positionals {
			var refs []string
			for _, ref := range installed {
				if plan.Matches(ref, arg) {
					refs = append(refs, ref)
				}
			}
			if len(refs) == 0 {
				return f.fail(1, "%s is not an app installed in the user installation", arg)
			}
			all = append(all, refs...)
		}
		if len(f.specs) > 0 && len(all) != 1 {
			return f.fail(2, "%s replaces the extra-data of one app at a time", extradata.Option)
		}
		for _, arg := range f.parsed.Positionals {
			for _, ref := range all {
				if !plan.Matches(ref, arg) {
					continue
				}
				d, _, err := fr.Active(f.user, ref)
				if err != nil {
					return f.fail(1, "%s: %v", ref, err)
				}
				upstream := d.Origin
				if upstream == fr.RemoteName {
					remote, _, ok := f.upstreamOf(ref)
					if !ok {
						return f.fail(1, "%s comes from %s but rpak has no record of its remote; reinstall it from its remote first", ref, fr.RemoteName)
					}
					upstream = remote
				}
				info, err := f.remoteInfo(upstream, ref)
				if err != nil {
					return f.fail(1, "%s: %v", ref, err)
				}
				if len(f.specs) > 0 {
					it, code, err := f.extraItem(arg, upstream, info)
					if err != nil {
						return f.fail(code, "%v", err)
					}
					if it.fromLocal {
						pending = append(pending, it)
					} else {
						items = append(items, it)
						moving[arg] = true
					}
					continue
				}
				it, ok, err := f.newItem(arg, upstream, info, f.parsed.ForceTarget)
				if err != nil {
					return f.fail(1, "%s: %v", ref, err)
				}
				if !ok {
					f.note("%s: upstream is already on %s, not forcing", idOf(ref), it.res.Original)
					if _, had := fr.Find(f.records, ref); !had {
						continue
					}
				}
				if it.method == fr.MethodReexport && !it.fromLocal {
					// Recorded once it is installed from rpak-local.
					if ok {
						items = append(items, it)
						moving[arg] = true
					}
					continue
				}
				// The record takes the new target from now on; what it
				// resolves to is applied below with the others.
				r, _ := fr.Find(f.records, ref)
				r.Ref, r.Target, r.Remote, r.Method = ref, it.target, upstream, it.method
				r.Original = it.res.Original.String()
				if r.Runtime == "" {
					r.Runtime = it.res.Original.String()
				}
				f.records = fr.Set(f.records, r)
			}
		}
	}

	// Reexported apps installed from rpak-local get a new export when
	// upstream has a new commit, the target resolves to another runtime or
	// an extra-data patch went; flatpak update then finds it. An app left
	// with no patch at all goes back to its remote.
	selected := f.parsed.Positionals
	if forced {
		selected = nil
		for _, arg := range f.parsed.Positionals {
			if !moving[arg] {
				selected = append(selected, arg)
			}
		}
	}
	stale := append([]item(nil), pending...)
	for _, ref := range f.reexported() {
		if !plan.MatchesAny(ref, f.parsed.Positionals) {
			continue
		}
		if _, ok := findItem(append(items, pending...), ref); ok {
			continue
		}
		it, ok := f.refresh(ref, items)
		if !ok {
			continue
		}
		m, exported := fr.Exported(f.user, ref)
		_, hasRemote := fr.FindRemote(f.user, fr.RemoteName)
		switch refreshAction(it, m, exported, hasRemote) {
		case goBack:
			f.back = append(f.back, it)
		case reexport:
			stale = append(stale, it)
		}
	}
	if len(stale) > 0 {
		if err := f.export(stale); err != nil {
			if len(pending) > 0 {
				return f.fail(1, "%v", err)
			}
			f.warn("%v; updating the reexported apps from what is exported", err)
			stale = nil
		}
		for _, it := range stale {
			if it.runtimePatch {
				r, _ := fr.Find(f.records, it.ref)
				if r.Ref == "" {
					r = reexportRecord(it)
				}
				r.Upstream, r.Original, r.Runtime = it.commit, it.res.Original.String(), it.res.Runtime.String()
				f.records = fr.Set(f.records, r)
			}
			f.extras = fr.SetExtra(f.extras, fr.ExtraRecord{Ref: it.ref, Remote: it.upstream, Upstream: it.commit, Sources: it.extra})
			f.announceItem(it)
		}
	}
	if forced && len(selected) == 0 {
		return f.apply("", false, items, nil, []string{})
	}
	return f.apply("", false, items, nil, selected)
}

// What becomes of a reexported app for upstream's latest commit, see
// refreshAction.
const (
	keepExport = iota // what is exported is what the patches want
	reexport          // export anew: upstream, the runtime or the extra-data changed
	goBack            // no patch left: install it from upstream again
)

// refreshAction decides what becomes of a reexported app, it being what
// its patches make of upstream's latest commit (its extra-data
// replacements already carried over, extradata.Carry), m what its export
// says it was made of (exported false: there is none), hasRemote whether
// rpak-local is configured. A force-runtime patch keeps the app on
// rpak-local even when its extra-data patches went.
func refreshAction(it item, m fr.Mark, exported, hasRemote bool) int {
	switch {
	case !it.runtimePatch && len(it.extra) == 0:
		return goBack
	case exported && hasRemote && m.Equal(it.job().Mark()):
		return keepExport
	}
	return reexport
}

// reexported lists the apps rpak reexports: those with a force-runtime
// record of method reexport or an extra-data record, sorted.
func (f *forceFlow) reexported() []string {
	var refs []string
	for _, r := range f.records {
		if r.Method == fr.MethodReexport {
			refs = append(refs, r.Ref)
		}
	}
	for _, r := range f.extras {
		if !slices.Contains(refs, r.Ref) {
			refs = append(refs, r.Ref)
		}
	}
	slices.Sort(refs)
	return refs
}

// refresh works out what a reexported app is to be made of now: upstream's
// latest commit with its recorded patches, those that still apply. ok is
// false when there is nothing to do: the app is not installed, is masked
// ("flatpak mask": flatpak would not update it, so rpak does not change
// what it records either), is not installed from rpak-local (it is not
// patched now; said so, unless items has it), or upstream cannot be read.
func (f *forceFlow) refresh(ref string, items []item) (item, bool) {
	d, ok, _ := fr.Active(f.user, ref)
	if !ok || fr.Masked(f.user, ref) {
		return item{}, false
	}
	r, hasRT := fr.Find(f.records, ref)
	hasRT = hasRT && r.Method == fr.MethodReexport
	remote, _, _ := f.upstreamOf(ref)
	if d.Origin != fr.RemoteName {
		if _, ok := findItem(items, ref); !ok {
			if hasRT {
				f.warn("%s is installed from %s, not %s, so its runtime is not forced; \"rpak update --force-runtime=%s %s\" forces it again", idOf(ref), d.Origin, fr.RemoteName, r.Target, idOf(ref))
			} else {
				f.warn("%s is installed from %s, not %s, so its extra-data are upstream's; \"rpak patch %s=URL %s\" replaces them again", idOf(ref), d.Origin, fr.RemoteName, extradata.Option, idOf(ref))
			}
		}
		return item{}, false
	}
	info, err := f.remoteInfo(remote, ref)
	if err != nil {
		f.warn("%s: cannot look at upstream %s, keeping what is exported: %v", idOf(ref), remote, err)
		return item{}, false
	}
	var it item
	if hasRT {
		it, ok, err = f.newItem(idOf(ref), remote, info, r.Target)
		if err == nil && !ok {
			f.note("%s: upstream is now on %s itself; \"rpak patch --reset=%s %s\" goes back to %s", idOf(ref), it.res.Original, fr.Kind, idOf(ref), remote)
		}
	} else {
		it, err = f.plainItem(idOf(ref), remote, info)
	}
	if err != nil {
		f.warn("%s: %v", ref, err)
		return item{}, false
	}
	it.method = fr.MethodReexport
	return it, true
}

func findItem(items []item, ref string) (item, bool) {
	for _, it := range items {
		if it.ref == ref {
			return it, true
		}
	}
	return item{}, false
}

// apply exports the items to reexport and runs flatpak: for install
// (updateArgs nil), "install REMOTE" with the patched and the plain refs,
// with --no-deps if any is patched; for update, "update" with updateArgs,
// with --no-deps if a patched app may be updated, unless updateArgs is
// empty but not nil (every argument named an app moving to rpak-local).
// Then the reexported apps not yet installed from rpak-local are installed
// from there. After success the patched apps are patched, the records
// saved, and runtimes that --no-deps left out installed.
func (f *forceFlow) apply(remote string, addUser bool, items, plain []item, updateArgs []string) int {
	var reexport []item
	for _, it := range items {
		if it.method == fr.MethodReexport {
			reexport = append(reexport, it)
		}
	}
	if len(reexport) > 0 {
		if err := f.export(reexport); err != nil {
			return f.fail(1, "%v", err)
		}
	}
	opts := append([]string(nil), f.options...)
	if addUser {
		opts = append(opts, args.User)
	}
	noDeps := false
	code := 0
	if f.kind == "install" {
		var refs []string
		for _, it := range items {
			if it.method == fr.MethodPatch {
				refs = append(refs, it.ref)
			}
		}
		for _, it := range plain {
			refs = append(refs, it.arg)
		}
		if len(refs) > 0 {
			a := append([]string(nil), opts...)
			if len(refs) > len(plain) && !hasOption(a, "--no-deps") {
				// Or flatpak pulls the runtime the commit names.
				a, noDeps = append(a, "--no-deps"), true
			}
			a = append(append(a, remote), refs...)
			code = f.runTransaction("install", f.cmdArgv("install", a...))
		}
		if code == 0 {
			for _, it := range items {
				if it.method == fr.MethodPatch {
					f.recordItem(it)
				}
			}
		}
	} else if updateArgs == nil || len(updateArgs) > 0 {
		a := append([]string(nil), opts...)
		noDeps = f.patchSelected(updateArgs) && !hasOption(a, "--no-deps")
		if noDeps && !f.parsed.ExplicitInstallation {
			code = f.updateSplit(a, updateArgs)
		} else {
			if noDeps {
				a = append(a, "--no-deps")
			}
			code = f.runTransaction("update", f.cmdArgv("update", append(a, updateArgs...)...))
		}
	}

	var toInstall []item
	for _, it := range reexport {
		if !it.fromLocal || f.kind == "install" {
			toInstall = append(toInstall, it)
		}
	}
	if code == 0 && len(toInstall) > 0 {
		a := installOptions(opts)
		reinstall := false
		for _, it := range toInstall {
			reinstall = reinstall || it.installed
		}
		if reinstall && !hasOption(a, "--reinstall") {
			// Another origin: flatpak uninstalls and installs again,
			// keeping ~/.var/app.
			a = append(a, "--reinstall")
		}
		a = append(a, fr.RemoteName)
		for _, it := range toInstall {
			a = append(a, it.ref)
		}
		code = f.runLocal("install", f.cmdArgv("install", a...))
		if code == 0 {
			for _, it := range toInstall {
				f.recordItem(it)
				f.announceItem(it)
			}
		}
	}
	var back []string
	if code == 0 && len(f.back) > 0 {
		back, code = f.backUpstream(opts)
	}
	// Also after a failure: what flatpak did deploy is patched.
	f.repatch()
	f.save()
	if len(back) > 0 {
		f.unexport(back)
	}
	if noDeps {
		f.fillRuntimes()
	}
	return code
}

// backUpstream installs the apps of f.back from their upstream remotes
// again, as reset does, and forgets their extra-data patches once that
// worked: upstream replaced the sources they replaced, nothing is patched
// any more. It returns the refs back upstream, to unexport.
func (f *forceFlow) backUpstream(opts []string) ([]string, int) {
	var records []fr.Record
	for _, it := range f.back {
		records = append(records, fr.Record{Ref: it.ref, Remote: it.upstream})
	}
	a := installOptions(opts)
	a = slices.DeleteFunc(a, func(o string) bool { return o == args.User || o == "-u" || o == "--reinstall" })
	code := 0
	var back []string
	for _, c := range resetCommands(records, a) {
		argv := f.cmdArgv(c.argv[0], c.argv[1:]...)
		if f.dryRun {
			f.dryCmd(argv)
		} else if c := f.runTransaction("install", argv); c != 0 {
			code = c
			continue
		}
		for _, r := range c.records {
			f.extras = fr.RemoveExtra(f.extras, r.Ref)
			back = append(back, r.Ref)
			if f.dryRun {
				f.dry("install %s from %s again: no patch left", idOf(r.Ref), r.Remote)
			} else {
				f.note("%s comes from %s again: no patch left", idOf(r.Ref), r.Remote)
			}
		}
	}
	return back, code
}

// patchSelected reports whether an update with these REF arguments may
// update an app forced by patching.
func (f *forceFlow) patchSelected(updateArgs []string) bool {
	for _, r := range f.records {
		if r.Method == fr.MethodPatch && plan.MatchesAny(r.Ref, updateArgs) {
			return true
		}
	}
	return false
}

// repatch makes the deployment of every app forced by patching run on
// its forced runtime again: after flatpak deployed a new commit its
// metadata is upstream's. Upstream's metadata comes from the commit's
// xa.metadata, which is never changed.
func (f *forceFlow) repatch() {
	for i, r := range f.records {
		if r.Method != fr.MethodPatch {
			continue
		}
		d, ok, err := fr.Active(f.user, r.Ref)
		if err != nil {
			f.warn("%s: %v", r.Ref, err)
			continue
		}
		if !ok || d.Origin == fr.RemoteName {
			continue
		}
		text, err := fr.CommitMetadata(f.user, d.Commit)
		var orig fr.Pref
		if err == nil {
			orig, err = fr.RuntimeOf(text)
		}
		var res fr.Resolution
		if err == nil {
			res, err = f.resolve(orig, r.Target)
		}
		if err == nil && res.Forced {
			text, err = fr.RewriteMetadata(text, orig, res.Runtime)
		}
		changed := false
		switch {
		case err != nil:
		case f.dryRun:
			if changed, err = fr.NeedsPatch(d, text); err == nil && changed {
				current := "?"
				if cur, err := d.MetadataFile(); err == nil {
					if p, err := fr.RuntimeOf(cur); err == nil {
						current = p.String()
					}
				}
				f.dry("rewrite metadata and deploy of %s (%s): runtime %s -> %s", idOf(r.Ref), d.Dir, current, res.Runtime)
			}
		default:
			changed, err = fr.Patch(d, text)
		}
		if err != nil {
			f.warn("%s: not forcing its runtime: %v", idOf(r.Ref), err)
			continue
		}
		f.records[i].Original, f.records[i].Runtime = orig.String(), res.Runtime.String()
		switch {
		case f.dryRun:
		case changed && res.Forced:
			f.announce(r.Ref, orig, res.Runtime)
		case changed:
			f.note("%s: upstream is now on %s itself, it runs on that", idOf(r.Ref), orig)
		}
	}
}

// fillRuntimes installs the runtimes the apps of the user installation
// name but that are installed nowhere: --no-deps applies to the whole
// transaction, so other apps it installed or updated may lack theirs.
// flatpak picks the remote.
func (f *forceFlow) fillRuntimes() {
	type need struct {
		ref     string
		runtime fr.Pref
	}
	var needs []need
	byRef := map[string]int{}
	for _, ref := range fr.Installed(f.user, "app") {
		d, ok, err := fr.Active(f.user, ref)
		if err != nil || !ok {
			continue
		}
		text, err := d.MetadataFile()
		if err != nil {
			continue
		}
		if p, err := fr.RuntimeOf(text); err == nil {
			byRef[ref] = len(needs)
			needs = append(needs, need{ref, p})
		}
	}
	if f.dryRun {
		// Nothing was patched: the patched apps need what their records
		// would make them run on. The reexported ones are installed from
		// rpak-local with their dependencies, by flatpak.
		for _, r := range f.records {
			if r.Method != fr.MethodPatch {
				if i, ok := byRef[r.Ref]; ok {
					needs[i].ref = ""
				}
				continue
			}
			p, err := fr.ParsePref(r.Runtime)
			if orig, err2 := fr.ParsePref(r.Original); err == nil && err2 == nil && p == orig {
				if res, err := f.resolve(orig, r.Target); err == nil {
					p = res.Runtime
				}
			}
			if err != nil {
				continue
			}
			if i, ok := byRef[r.Ref]; ok {
				needs[i].runtime = p
			} else {
				needs = append(needs, need{r.Ref, p})
			}
		}
	}
	seen := map[fr.Pref]bool{}
	for _, n := range needs {
		p, ref := n.runtime, n.ref
		if ref == "" || seen[p] || fr.RuntimeInstalled(f.insts, p) {
			continue
		}
		seen[p] = true
		argv := []string{"install", "--user", "-y", "--noninteractive", p.Ref()}
		if f.dryRun {
			f.dry("install %s, which %s needs", p.Ref(), idOf(ref))
			f.dryCmd(argv)
			continue
		}
		f.note("installing %s, which %s needs", p.Ref(), idOf(ref))
		if code := runAttached(f.flatpak, argv); code != 0 {
			f.warn("installing %s failed; %s will not start until it is installed", p.Ref(), idOf(ref))
		}
	}
}

// runAttached runs flatpak on rpak's terminal and returns its exit code.
func runAttached(flatpak string, argv []string) int {
	cmd := exec.Command(flatpak, argv...)
	cmd.Args[0] = "flatpak"
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "rpak: cannot run flatpak: %v\n", err)
		return 127
	}
	return exitCode(cmd.Wait())
}

// installOptions keeps the options flatpak install takes too, for the
// install from rpak-local that an update ends with.
func installOptions(opts []string) []string {
	var out []string
	for i := 0; i < len(opts); i++ {
		o := opts[i]
		name, _, hasValue := strings.Cut(o, "=")
		if !strings.HasPrefix(o, "--") || args.InstallTakes(name) {
			out = append(out, o)
			if args.TakesValue("install", name) && !hasValue && i+1 < len(opts) {
				i++
				out = append(out, opts[i])
			}
		} else if args.TakesValue("update", name) && !hasValue {
			i++
		}
	}
	return out
}

func hasOption(opts []string, name string) bool {
	for _, o := range opts {
		if o == name {
			return true
		}
	}
	return false
}

// idOf is the id of a full ref.
func idOf(ref string) string {
	if parts := strings.Split(ref, "/"); len(parts) == 4 {
		return parts[1]
	}
	return ref
}

// exportText says what an export changes: "runtime R", "extra-data NAME",
// both.
func exportText(it item) string {
	parts := []string{"runtime " + it.res.Runtime.String()}
	for _, e := range it.extra {
		parts = append(parts, fr.ExtraKind+" "+e.Name)
	}
	return strings.Join(parts, ", ")
}

// export reexports the items into the local repository and makes sure its
// remote is there, adding it the first time.
func (f *forceFlow) export(items []item) error {
	if f.dryRun {
		for _, it := range items {
			f.dry("reexport %s %s from %s with %s into %s", idOf(it.ref), it.commit[:12], it.upstream, exportText(it), fr.RepoPath(f.user))
			f.dryCmd(fr.ExportPreview(f.user, it.job()))
		}
		f.dryCmd(fr.PublishArgv(f.user))
		if _, ok := fr.FindRemote(f.user, fr.RemoteName); !ok {
			f.dryCmd(fr.RemoteAddArgv(f.user))
		}
		return nil
	}
	remotes := f.userRemotes()
	srcs := map[string]ostree.Source{}
	for _, it := range items {
		src := srcs[it.upstream]
		if src == nil {
			rem, ok := remotes[it.upstream]
			if !ok {
				return fmt.Errorf("the user installation has no remote %s", it.upstream)
			}
			var err error
			if src, err = ostree.NewSource(rem.URL, prefetch.NewHTTPClient(), prefetch.UserAgent(f.version)); err != nil {
				return fmt.Errorf("remote %s: %w", it.upstream, err)
			}
			srcs[it.upstream] = src
		}
		f.note("reexporting %s %s from %s with %s", idOf(it.ref), it.commit[:12], it.upstream, exportText(it))
		if err := fr.Export(f.ctx, f.query, f.user, src, it.job()); err != nil {
			return fmt.Errorf("reexporting %s: %w", it.ref, err)
		}
	}
	if err := fr.Publish(f.ctx, f.query, f.user); err != nil {
		return fmt.Errorf("updating the local repository: %w", err)
	}
	added, err := fr.EnsureRemote(f.ctx, f.query, f.user)
	if err != nil {
		return fmt.Errorf("adding the remote %s: %w", fr.RemoteName, err)
	}
	if added {
		f.note("added the remote %s for the reexported apps (%s)", fr.RemoteName, fr.RepoPath(f.user))
	}
	return nil
}

// updateSplit runs an update of every installation in which a patched app
// may be updated as two: the user installation with --no-deps, the system
// installations without, so that their apps still get the runtimes they
// move to. With REF arguments each gets those of its refs that are
// installed there; arguments installed nowhere go to the user one, for
// flatpak to report.
func (f *forceFlow) updateSplit(opts, updateArgs []string) int {
	var systems []installation.Installation
	for _, inst := range (installation.Env{}).All() {
		if inst.User {
			continue
		}
		if fi, err := os.Stat(inst.Path); err == nil && fi.IsDir() {
			systems = append(systems, inst)
		}
	}
	installedIn := func(path, arg string) bool {
		for _, kind := range []string{"app", "runtime"} {
			for _, ref := range fr.Installed(path, kind) {
				if plan.Matches(ref, arg) {
					return true
				}
			}
		}
		return false
	}
	var userArgs, sysArgs []string
	for _, arg := range updateArgs {
		inSystem := false
		for _, inst := range systems {
			inSystem = inSystem || installedIn(inst.Path, arg)
		}
		if inSystem {
			sysArgs = append(sysArgs, arg)
		}
		if installedIn(f.user, arg) || !inSystem {
			userArgs = append(userArgs, arg)
		}
	}
	code := 0
	if len(updateArgs) == 0 || len(userArgs) > 0 {
		a := append(append([]string(nil), opts...), args.User, "--no-deps")
		code = f.runTransaction("update", f.cmdArgv("update", append(a, userArgs...)...))
	}
	if len(systems) > 0 && (len(updateArgs) == 0 || len(sysArgs) > 0) {
		a := append([]string(nil), opts...)
		for _, inst := range systems {
			a = append(a, inst.Option())
		}
		if c := f.runTransaction("update", f.cmdArgv("update", append(a, sysArgs...)...)); code == 0 {
			code = c
		}
	}
	return code
}

// patchApps is rpak patch --force-runtime[=TARGET] and/or
// --extra-data=[NAME=]URL APP...: the installed apps of the user
// installation are patched as they are, without updating them. A patched
// app is rewritten in place; an app with extra-data has the commit it has
// installed reexported, and is reinstalled from rpak-local (or updated
// there, if it comes from it already).
func patchApps(version string, globals, opts, apps []string, force bool, target string, extra []string, stderr io.Writer) int {
	if force {
		if err := fr.ValidTarget(target); err != nil {
			fmt.Fprintf(stderr, "rpak: %v\n", err)
			return 2
		}
	}
	parsed := args.Parsed{Command: "patch", Positionals: apps, ForceRuntime: force, ForceTarget: target, ExtraData: extra,
		Installations: []string{args.User}, ExplicitInstallation: true}
	argv := append(append(append([]string(nil), globals...), "patch"), opts...)
	f, code, ok := newForceFlow(version, "patch", argv, len(globals), parsed, stderr)
	if !ok {
		return code
	}
	defer f.close()
	f.options = append(append([]string(nil), opts...), args.User)
	f.dropStale()

	installed := fr.Installed(f.user, "app")
	var all []string
	for _, arg := range apps {
		n := len(all)
		for _, ref := range installed {
			if plan.Matches(ref, arg) && !slices.Contains(all, ref) {
				all = append(all, ref)
			}
		}
		if len(all) == n {
			return f.fail(1, "%s is not an app installed in the user installation", arg)
		}
	}
	if len(f.specs) > 0 && len(all) != 1 {
		return f.fail(2, "%s replaces the extra-data of one app at a time", extradata.Option)
	}
	var items []item
	for _, ref := range all {
		info, upstream, err := f.installedInfo(ref)
		if err != nil {
			return f.fail(1, "%s: %v", ref, err)
		}
		if len(f.specs) > 0 {
			it, code, err := f.extraItem(idOf(ref), upstream, info)
			if err != nil {
				return f.fail(code, "%v", err)
			}
			items = append(items, it)
			continue
		}
		it, ok, err := f.newItem(idOf(ref), upstream, info, target)
		if err != nil {
			return f.fail(1, "%s: %v", ref, err)
		}
		r, had := fr.Find(f.records, ref)
		if !ok {
			f.note("%s: it is built for %s already, not forcing", idOf(ref), it.res.Original)
			if !had {
				continue
			}
		}
		if it.method == fr.MethodReexport && (ok || len(it.extra) > 0) {
			it.runtimePatch = ok || had
			items = append(items, it)
			continue
		}
		// Patched in place below (repatch); a target that is no
		// upgrade puts the app back as built.
		r.Ref, r.Target, r.Remote, r.Method = ref, it.target, upstream, it.method
		r.Original, r.Runtime = it.res.Original.String(), it.res.Original.String()
		f.records = fr.Set(f.records, r)
	}
	return f.applyPatch(items)
}

// installedInfo is the installed commit of ref and its upstream metadata,
// and the upstream remote: for an app from rpak-local, the commit it was
// exported from, read from upstream.
func (f *forceFlow) installedInfo(ref string) (remoteRef, string, error) {
	d, _, err := fr.Active(f.user, ref)
	if err != nil {
		return remoteRef{}, "", err
	}
	if d.Origin != fr.RemoteName {
		text, err := fr.CommitMetadata(f.user, d.Commit)
		return remoteRef{ref: ref, commit: d.Commit, metadata: text}, d.Origin, err
	}
	remote, upstream, ok := f.upstreamOf(ref)
	if !ok || upstream == "" {
		return remoteRef{}, "", fmt.Errorf("it comes from %s but rpak has no record of where from; reinstall it from its remote", fr.RemoteName)
	}
	rem, ok := f.userRemotes()[remote]
	if !ok {
		return remoteRef{}, "", fmt.Errorf("the user installation has no remote %s", remote)
	}
	src, err := ostree.NewSource(rem.URL, prefetch.NewHTTPClient(), prefetch.UserAgent(f.version))
	if err != nil {
		return remoteRef{}, "", err
	}
	c, err := ostree.ReadCommit(f.ctx, src, upstream)
	if err != nil {
		return remoteRef{}, "", err
	}
	text, ok, err := c.MetadataString("xa.metadata")
	if err == nil && !ok {
		err = fmt.Errorf("upstream's commit %s has no metadata", upstream[:12])
	}
	return remoteRef{ref: ref, commit: upstream, metadata: text}, remote, err
}

// applyPatch finishes rpak patch: the reexported apps are exported, then
// reinstalled from rpak-local, or updated there if they come from it
// already; then every patched app is patched, the records saved, and
// missing runtimes installed (the target may be one).
func (f *forceFlow) applyPatch(items []item) int {
	var export, reinstall, update []item
	for _, it := range items {
		if it.fromLocal {
			if m, ok := fr.Exported(f.user, it.ref); ok && m.Equal(it.job().Mark()) {
				f.recordItem(it)
				continue
			}
			update = append(update, it)
		} else {
			reinstall = append(reinstall, it)
		}
		export = append(export, it)
	}
	code := 0
	if len(export) > 0 {
		if err := f.export(export); err != nil {
			return f.fail(1, "%v", err)
		}
	}
	run := func(cmd string, its []item, extra ...string) {
		if len(its) == 0 || code != 0 {
			return
		}
		a := append(append([]string(nil), f.options...), extra...)
		if cmd == "install" {
			a = append(a, fr.RemoteName)
		}
		for _, it := range its {
			a = append(a, it.ref)
		}
		code = f.runLocal(cmd, f.cmdArgv(cmd, a...))
		if code == 0 {
			for _, it := range its {
				f.recordItem(it)
				f.announceItem(it)
			}
		}
	}
	// Another origin: flatpak uninstalls and installs again, keeping
	// ~/.var/app.
	run("install", reinstall, "--reinstall")
	run("update", update)
	f.repatch()
	f.save()
	f.fillRuntimes()
	return code
}

func reexportRecord(it item) fr.Record {
	return fr.Record{
		Ref: it.ref, Target: it.target, Remote: it.upstream, Method: fr.MethodReexport,
		Original: it.res.Original.String(), Runtime: it.res.Runtime.String(), Upstream: it.commit,
	}
}
