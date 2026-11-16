package doctor

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/flatpark/rpak/internal/clean"
	"github.com/flatpark/rpak/internal/installation"
	"github.com/flatpark/rpak/internal/lock"
	"github.com/flatpark/rpak/internal/prefetch"
	"github.com/flatpark/rpak/internal/units"
)

// Remote is a line of `flatpak remotes --columns=name,url,options,priority`.
type Remote struct {
	Name, URL string
	Options   []string
	Priority  string
}

// Has reports whether the remote has an option ("disabled", ...).
func (r Remote) Has(option string) bool { return contains(r.Options, option) }

// ParseRemotes reads flatpak's tab-separated remotes table.
func ParseRemotes(out string) []Remote {
	var remotes []Remote
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, "\t")
		for len(f) < 4 {
			f = append(f, "")
		}
		var opts []string
		for _, o := range strings.Split(f[2], ",") {
			if o = strings.TrimSpace(o); o != "" {
				opts = append(opts, o)
			}
		}
		remotes = append(remotes, Remote{Name: strings.TrimSpace(f[0]), URL: strings.TrimSpace(f[1]), Options: opts, Priority: strings.TrimSpace(f[3])})
	}
	return remotes
}

// RemoteProblem is a judgement on the remotes of an installation.
type RemoteProblem struct {
	Remote string // "" for the installation as a whole
	Text   string
	Hint   string
}

// CheckRemotes judges an installation's remotes by their options: a
// disabled remote gives nothing, and one without GPG verification is not
// checked against a signing key. A local repository (file:, a build or
// an installed bundle) is normally unsigned, so that is not a problem
// there. inst is the option of the installation, for the hints.
func CheckRemotes(remotes []Remote, inst string) []RemoteProblem {
	var out []RemoteProblem
	for _, r := range remotes {
		if r.Has("disabled") {
			out = append(out, RemoteProblem{r.Name, fmt.Sprintf("remote %s is disabled", r.Name),
				fmt.Sprintf("flatpak remote-modify %s --enable %s", inst, r.Name)})
		}
		if r.Has("no-gpg-verify") && !strings.HasPrefix(r.URL, "file:") {
			out = append(out, RemoteProblem{r.Name, fmt.Sprintf("remote %s is not GPG-verified (no-gpg-verify)", r.Name),
				fmt.Sprintf("add it again from its .flatpakrepo file, which carries the key; or flatpak remote-modify %s --gpg-import=KEYFILE %s", inst, r.Name)})
		}
	}
	return out
}

// MissingRemotes judges the remotes of all installations in use together,
// by name only: FlatPark's apps need a flatpark remote, their runtimes
// mostly a flathub one. Either may be in any installation, since apps use
// runtimes from other installations too; only if none has one is it
// missing, and the hint is the per-user setup of FlatPark's docs.
func MissingRemotes(remoteLists [][]Remote) []RemoteProblem {
	have := map[string]bool{}
	for _, list := range remoteLists {
		for _, r := range list {
			have[r.Name] = true
		}
	}
	var out []RemoteProblem
	if !have["flathub"] {
		out = append(out, RemoteProblem{"", "no remote named flathub in any installation: the runtimes of most apps come from Flathub",
			"flatpak remote-add --user --if-not-exists flathub https://dl.flathub.org/repo/flathub.flatpakrepo"})
	}
	if !have["flatpark"] {
		out = append(out, RemoteProblem{"", "no remote named flatpark in any installation",
			"flatpak remote-add --user --if-not-exists flatpark https://dl.flatpark.org/flatpark.flatpakrepo (https://flatpark.org/docs/users/setup/)"})
	}
	return out
}

// listEntry is a line of `flatpak list --columns=ref,options`.
type listEntry struct {
	ref     string
	options string // comma-separated: installation, current, runtime, ..., eol=REASON, eol-rebase=REF
}

// eol is the end-of-life reason of the entry. flatpak puts eol= and
// eol-rebase= last, and a reason may contain commas, so it runs up to
// ",eol-rebase=" or the end.
func (e listEntry) eol() (string, bool) {
	opts := "," + e.options
	i := strings.Index(opts, ",eol=")
	if i < 0 {
		return "", false
	}
	reason := opts[i+len(",eol="):]
	reason, _, _ = strings.Cut(reason, ",eol-rebase=")
	return reason, true
}

// parseList reads `flatpak list --columns=ref,options`.
func parseList(out string) []listEntry {
	var list []listEntry
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		ref, opts, _ := strings.Cut(line, "\t")
		list = append(list, listEntry{ref: strings.TrimSpace(ref), options: strings.TrimSpace(opts)})
	}
	return list
}

type refList struct {
	refs []listEntry
	err  error
}

// LockState is the state of one of an installation's lock files.
type LockState struct {
	Path string
	Busy bool
	PID  int // > 0 if known
	Err  error
}

type instInfo struct {
	inst    installation.Installation
	exists  bool
	statErr error

	writable bool
	free     uint64
	freeErr  error
	locks    []LockState

	remotes    []Remote
	remotesErr error
	apps       refList
	runtimes   refList

	global    string // overrides/global, if it exists
	hasGlobal bool
	overrides []string // ids with their own override
}

// installations discovers the installations as flatpak does, and looks at
// each of those that exist.
func (d *doctor) installations() []*instInfo {
	env := d.installEnv()
	var list []installation.Installation
	if u, err := env.User(); err == nil {
		list = append(list, u)
	}
	list = append(list, env.SystemDefault())
	configured, _ := env.Configured()
	list = append(list, configured...)

	out := make([]*instInfo, len(list))
	var fns []func()
	for i, inst := range list {
		in := &instInfo{inst: inst}
		out[i] = in
		info, err := os.Stat(inst.Path)
		if err != nil || !info.IsDir() {
			in.statErr = err
			continue
		}
		in.exists = true
		fns = append(fns, func() { d.lookAt(in) })
	}
	parallel(fns...)
	return out
}

func (d *doctor) lookAt(in *instInfo) {
	path, opt := in.inst.Path, in.inst.Option()
	parallel(
		func() {
			in.writable = unix.Access(path, unix.W_OK) == nil
			var st unix.Statfs_t
			if err := unix.Statfs(path, &st); err != nil {
				in.freeErr = err
			} else {
				in.free = st.Bavail * uint64(st.Bsize)
			}
			for _, f := range []string{filepath.Join(path, "repo", ".lock"), filepath.Join(path, "repo-lock")} {
				in.locks = append(in.locks, d.probeLock(f))
			}
			in.global, in.hasGlobal, in.overrides = ReadOverrides(filepath.Join(path, "overrides"))
		},
		func() {
			out, err := d.query("remotes", opt, "--columns=name,url,options,priority")
			in.remotes, in.remotesErr = ParseRemotes(out), err
		},
		func() {
			out, err := d.query("list", opt, "--app", "--columns=ref,options")
			in.apps = refList{parseList(out), err}
		},
		func() {
			out, err := d.query("list", opt, "--runtime", "--columns=ref,options")
			in.runtimes = refList{parseList(out), err}
		},
	)
}

// ReadOverrides reads an installation's overrides directory: the global
// override's text, and the ids of the apps with their own override.
func ReadOverrides(dir string) (global string, hasGlobal bool, ids []string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false, nil
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if e.Name() == "global" {
			text, err := os.ReadFile(filepath.Join(dir, "global"))
			if err == nil {
				global, hasGlobal = string(text), true
			}
			continue
		}
		ids = append(ids, e.Name())
	}
	sort.Strings(ids)
	return global, hasGlobal, ids
}

// ReadOverride is the text of an app's override in an overrides
// directory; ok is false if it has none.
func ReadOverride(dir, id string) (text string, ok bool) {
	if id == "" || strings.ContainsAny(id, "/\x00") || id == "." || id == ".." {
		return "", false
	}
	b, err := os.ReadFile(filepath.Join(dir, id))
	if err != nil {
		return "", false
	}
	return string(b), true
}

// probeLock checks a lock file without locking it (lock.Probe). Where the
// file cannot be opened (a system installation's, for a user) or the
// kernel does not name the holder (OFD locks), /proc/locks is asked by
// inode, which needs no access to the file.
func (d *doctor) probeLock(path string) LockState {
	st := LockState{Path: path}
	busy, pid, err := lock.Probe(path)
	if err != nil {
		if !errors.Is(err, fs.ErrPermission) {
			st.Err = err
			return st
		}
		var s unix.Stat_t
		if err := unix.Stat(path, &s); err != nil {
			st.Err = err
			return st
		}
		holders, err := lockHolders(d.path("/proc/locks"), s.Ino)
		if err != nil {
			st.Err = err
			return st
		}
		st.Busy = len(holders) > 0
		if st.Busy {
			st.PID = holders[0]
		}
		return st
	}
	st.Busy = busy
	if busy && pid > 0 {
		st.PID = pid
	} else if busy {
		var s unix.Stat_t
		if unix.Stat(path, &s) == nil {
			if holders, _ := lockHolders(d.path("/proc/locks"), s.Ino); len(holders) > 0 {
				st.PID = holders[0]
			}
		}
	}
	return st
}

// lockHolders returns the pids that /proc/locks lists for an inode (0 or
// -1 where the kernel does not know them are left out, but the lock still
// counts: the result is then [-1]). Lines look like
// "1: OFDLCK ADVISORY  READ  1234 00:24:48851 0 EOF"; "->" marks waiters.
func lockHolders(procLocks string, ino uint64) ([]int, error) {
	f, err := os.Open(procLocks)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var pids []int
	unknown := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) > 1 && fields[1] == "->" {
			continue
		}
		if len(fields) < 6 {
			continue
		}
		dev := strings.Split(fields[5], ":")
		if len(dev) != 3 {
			continue
		}
		if n, err := strconv.ParseUint(dev[2], 10, 64); err != nil || n != ino {
			continue
		}
		pid, err := strconv.Atoi(fields[4])
		if err != nil || pid <= 0 {
			unknown = true
			continue
		}
		pids = append(pids, pid)
	}
	if len(pids) == 0 && unknown {
		pids = []int{-1}
	}
	return pids, sc.Err()
}

// commName is the name of a process, for lock holders.
func (d *doctor) commName(pid int) string {
	b, err := os.ReadFile(d.path(fmt.Sprintf("/proc/%d/comm", pid)))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// used reports whether an installation is in use: it has remotes or
// something installed. A system installation nobody uses has neither.
func (in *instInfo) used() bool {
	return len(in.remotes) > 0 || len(in.apps.refs) > 0 || len(in.runtimes.refs) > 0
}

// lowSpace is when free space becomes a problem: apps and their updates
// often need more.
const lowSpace = 2 << 30

func (d *doctor) installationsSection(insts []*instInfo) Section {
	s := d.section("Installations")
	r := d.r
	for _, in := range insts {
		s.head(in.inst.Title(), "%s: %s", in.inst.Title(), in.inst.Path)
		if !in.exists {
			if in.statErr != nil && !errors.Is(in.statErr, fs.ErrNotExist) {
				s.add(Warn, "cannot read the installation: %v", in.statErr).hint(r, "check the permissions of %s", in.inst.Path)
			} else {
				s.add(Info, "does not exist")
			}
			continue
		}
		access := "read-only for this user"
		if in.writable {
			access = "writable"
		}
		switch {
		case in.freeErr != nil:
			s.add(Info, "%s; free space unknown: %v", access, in.freeErr)
		case in.free < lowSpace:
			s.add(Warn, "%s; only %s free", access, units.Size(int64(in.free))).
				hint(r, "free some space; \"rpak clean\" removes what flatpak leaves behind")
		default:
			s.add(OK, "%s; %s free", access, units.Size(int64(in.free)))
		}
		for _, l := range in.locks {
			name := filepath.Base(filepath.Dir(l.Path)) + "/" + filepath.Base(l.Path)
			if filepath.Base(l.Path) == "repo-lock" {
				name = "repo-lock"
			}
			switch {
			case l.Err != nil:
				s.add(Info, "%s: cannot check the lock: %v", name, l.Err)
			case l.Busy && l.PID > 0:
				s.add(Warn, "%s is locked by pid %d (%s): another flatpak transaction is running", name, l.PID, d.commName(l.PID)).
					hint(r, "wait for it (a software center or an automatic update) to finish, then try again")
			case l.Busy:
				s.add(Warn, "%s is locked (holder unknown): another flatpak transaction is running", name).
					hint(r, "wait for it (a software center or an automatic update) to finish, then try again")
			default:
				s.add(OK, "%s is not locked", name)
			}
		}

		if errors.Is(in.remotesErr, errNoFlatpak) {
			// Said once, with the flatpak line.
			s.add(Info, "remotes and refs not checked: %v", in.remotesErr)
		} else {
			d.refsItems(s, in)
		}
		d.overrideItems(s, in)
	}

	// Across the installations, if their remotes could be listed at all.
	var lists [][]Remote
	listed := false
	for _, in := range insts {
		if in.exists && in.remotesErr == nil {
			listed = true
			if in.used() {
				lists = append(lists, in.remotes)
			}
		}
	}
	if listed {
		s.head("Installations", "All installations")
		problems := MissingRemotes(lists)
		for _, p := range problems {
			s.add(Warn, "%s", p.Text).hint(r, "%s", p.Hint)
		}
		if len(problems) == 0 {
			s.add(OK, "remotes named flathub and flatpark are configured")
		}
	}
	return s.done()
}

// refsItems are the remotes and what is installed.
func (d *doctor) refsItems(s *builder, in *instInfo) {
	r := d.r
	if in.remotesErr != nil {
		s.add(Warn, "cannot list the remotes: %v", in.remotesErr).hint(r, "flatpak repair %s", in.inst.Option())
	}
	for _, rem := range in.remotes {
		opts := strings.Join(rem.Options, ",")
		if opts != "" {
			opts = ", " + opts
		}
		s.add(Info, "remote %s  %s  (priority %s%s)", rem.Name, RedactURL(rem.URL), rem.Priority, opts)
	}
	if in.remotesErr == nil && in.used() {
		for _, p := range CheckRemotes(in.remotes, in.inst.Option()) {
			s.add(Warn, "%s", p.Text).hint(r, "%s", p.Hint)
		}
	}

	counts := func(l refList, what string) string {
		if l.err != nil {
			return what + ": unknown (" + l.err.Error() + ")"
		}
		return fmt.Sprintf("%d %s", len(l.refs), what)
	}
	s.add(Info, "%s, %s", counts(in.apps, "apps"), counts(in.runtimes, "runtimes"))
	for _, l := range []refList{in.runtimes, in.apps} {
		for _, e := range l.refs {
			if reason, ok := e.eol(); ok {
				s.add(Warn, "%s is end-of-life: %s", e.ref, reason).
					hint(r, "flatpak update %s; if the apps still need it, they have to move on first; \"flatpak uninstall %s --unused\" removes unused runtimes", in.inst.Option(), in.inst.Option())
			}
		}
	}
}

// overrideItems are the overrides, which flatpak is not needed to read.
func (d *doctor) overrideItems(s *builder, in *instInfo) {
	r := d.r
	if in.hasGlobal {
		s.add(Warn, "global override (%s): applies to every app", filepath.Join(in.inst.Path, "overrides", "global")).
			block(r, strings.TrimRight(in.global, "\n")).
			hint(r, "a global override is the most common source of surprises; review it with \"flatpak override %s --show\", remove it with \"flatpak override %s --reset\" (https://flatpark.org/docs/users/override/)", in.inst.Option(), in.inst.Option())
	}
	if len(in.overrides) > 0 {
		s.add(Info, "apps with an override: %s", strings.Join(in.overrides, ", "))
	}
}

// --- the extra-data directory

type extraDataInfo struct {
	dir     string
	exists  bool
	err     error
	entries int
	size    int64
	parts   int
	partSz  int64

	// what `rpak clean --dry-run` would free; cleanDone is false if it took
	// longer than CleanTimeout.
	cleanDone bool
	freeable  int64
}

func (d *doctor) extraData(insts []*instInfo) extraDataInfo {
	var x extraDataInfo
	base, err := prefetch.UserBaseDir(d.getenv, func() (string, error) { return d.cfg.Home, nil })
	if err != nil {
		x.err = err
		return x
	}
	x.dir = filepath.Join(base, "extra-data")
	done := make(chan int64, 1)
	go func() { done <- dryRunClean(insts) }()

	entries, err := os.ReadDir(x.dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		x.err = err
	default:
		x.exists = true
		x.entries = len(entries)
		filepath.WalkDir(x.dir, func(p string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return nil
			}
			info, err := e.Info()
			if err != nil {
				return nil
			}
			x.size += info.Size()
			if strings.HasSuffix(e.Name(), ".part") {
				x.parts++
				x.partSz += info.Size()
			}
			return nil
		})
	}

	timeout := time.NewTimer(d.cfg.CleanTimeout)
	defer timeout.Stop()
	select {
	case x.freeable = <-done:
		x.cleanDone = true
	case <-timeout.C:
	case <-d.ctx.Done():
	}
	return x
}

// dryRunClean is what `rpak clean --dry-run` counts, without the data of
// uninstalled apps, which is only deleted on request: a dry run of each
// clean-up, which reads and changes nothing, and takes no locks.
func dryRunClean(insts []*instInfo) int64 {
	var total int64
	for _, in := range insts {
		if !in.exists {
			continue
		}
		c := &clean.Cleaner{DryRun: true}
		if clean.HasRepo(in.inst.Path) {
			repo := filepath.Join(in.inst.Path, "repo")
			c.StripExtraData(repo)
			c.RepoLeftovers(repo)
			c.InterruptedDeploys(in.inst.Path)
			c.Removed(in.inst.Path)
		}
		if in.inst.User {
			c.PrefetchDirs(in.inst.Path)
		}
		total += c.Freed
	}
	return total
}

func (d *doctor) extraDataSection(x extraDataInfo) Section {
	s := d.section("Extra-data directory")
	r := d.r
	switch {
	case x.dir == "":
		s.add(Warn, "cannot find the flatpak user directory: %v", x.err).hint(r, "set HOME")
		return s.done()
	case x.err != nil:
		s.add(Warn, "%s: %v", x.dir, x.err).hint(r, "check the permissions of %s", x.dir)
	case !x.exists:
		s.add(OK, "%s: does not exist (nothing prefetched)", x.dir)
	default:
		s.add(Info, "%s: %d entries, %s", x.dir, x.entries, units.Size(x.size))
		if x.parts > 0 {
			s.add(Warn, "%d partial downloads (.part), %s", x.parts, units.Size(x.partSz)).
				hint(r, "they are resumed by the next install or update; \"rpak clean\" removes them")
		}
	}
	switch {
	case !x.cleanDone:
		s.add(Info, "what \"rpak clean\" can free takes long to work out; see \"rpak clean --dry-run\"")
	case x.freeable > 0:
		s.add(Info, "\"rpak clean\" can free %s (see \"rpak clean --dry-run\")", units.Size(x.freeable))
	default:
		s.add(OK, "\"rpak clean\" has nothing to free")
	}
	return s.done()
}
