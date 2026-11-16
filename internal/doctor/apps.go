package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/flatpark/rpak/internal/metadata"
	"github.com/flatpark/rpak/internal/prefetch"
	"github.com/flatpark/rpak/internal/summary"
	"github.com/flatpark/rpak/internal/units"
)

// ParseInfo reads the "Key: value" lines of `flatpak info`.
func ParseInfo(out string) map[string]string {
	info := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(line, ":")
		key = strings.TrimSpace(key)
		if !ok || key == "" || strings.Contains(key, " ") && key != "Installed Size" {
			continue
		}
		info[key] = strings.TrimSpace(value)
	}
	return info
}

// ExtraDataCheck is the judgement on one extra-data file of an app.
type ExtraDataCheck struct {
	Source metadata.Source
	URL    URLCheck
	Cached bool // complete in the extra-data directory already
}

// Judge says what the check means: Fail if flatpak will not get the file
// (no 2xx answer, or a size other than the pinned one, which means the
// developer replaced the file), OK otherwise.
func (c ExtraDataCheck) Judge() (Level, string) {
	switch {
	case !c.URL.OK():
		return Fail, c.URL.Problem()
	case c.URL.Length >= 0 && c.URL.Length != c.Source.Size:
		return Fail, "the server has " + units.Size(c.URL.Length) + ", pinned " + units.Size(c.Source.Size) +
			": the developer replaced the file and the pin is not updated yet"
	case c.URL.Length < 0:
		return OK, "HTTP " + strconv.Itoa(c.URL.Status) + ", size not given, pinned " + units.Size(c.Source.Size)
	}
	return OK, "HTTP " + strconv.Itoa(c.URL.Status) + ", " + units.Size(c.Source.Size) + " as pinned"
}

// commitCheck is the extra-data check of one commit of an app.
type commitCheck struct {
	// label says which commit it is: "installed and remote commit",
	// "remote commit, pending update", ...
	label   string
	extra   []ExtraDataCheck
	skipped []metadata.Skipped
}

// appInst is an app in one installation.
type appInst struct {
	in      *instInfo
	info    map[string]string
	err     error // other than "not installed"
	perms   string
	permErr error
	// metaErr: the installed commit's metadata cannot be read; remoteErr:
	// the origin's summary cannot be, or has no such ref.
	metaErr   error
	remoteErr error
	checks    []commitCheck
}

// remoteFind is an app that is not installed, as found in one remote.
type remoteFind struct {
	remote string // "flatpark (User installation)"
	check  commitCheck
}

type appInfo struct {
	id    string
	insts []*appInst // installed in, in installation order
	// checked is false if no installation could be asked.
	checked bool

	// For an app installed nowhere: where it was found, and the remotes
	// whose summary could not be read.
	found      []remoteFind
	unreadable []string

	perms    string // flatpak permission-show, see PermissionStore
	permsErr error
	dataDir  string
	dataSize int64
	dataErr  error
	dataBig  bool // measuring took longer than DuTimeout
	hasData  bool
}

type psInfo struct {
	pids map[string][]string
	err  error
}

func (d *doctor) ps() psInfo {
	if len(d.cfg.Apps) == 0 {
		return psInfo{}
	}
	out, err := d.query("ps", "--columns=application,pid")
	p := psInfo{pids: map[string][]string{}, err: err}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 {
			p.pids[f[0]] = append(p.pids[f[0]], f[1])
		}
	}
	return p
}

// apps looks at each app on the command line in each installation.
func (d *doctor) apps(insts []*instInfo) []*appInfo {
	if len(d.cfg.Apps) == 0 {
		return nil
	}
	var existing []*instInfo
	for _, in := range insts {
		if in.exists {
			existing = append(existing, in)
		}
	}
	out := make([]*appInfo, len(d.cfg.Apps))
	var fns []func()
	for i, id := range d.cfg.Apps {
		a := &appInfo{id: id}
		out[i] = a
		fns = append(fns, func() { d.app(a, existing) })
	}
	parallel(fns...)
	return out
}

func (d *doctor) app(a *appInfo, insts []*instInfo) {
	found := make([]*appInst, len(insts))
	var fns []func()
	for i, in := range insts {
		fns = append(fns, func() {
			out, err := d.query("info", in.inst.Option(), "--", a.id)
			if err != nil {
				if strings.Contains(err.Error(), "not installed") {
					return
				}
				found[i] = &appInst{in: in, err: err}
				return
			}
			ai := &appInst{in: in, info: ParseInfo(out)}
			found[i] = ai
			d.appDetails(a.id, ai)
		})
	}
	fns = append(fns,
		func() {
			out, err := d.query("permission-show", "--", a.id)
			a.perms, a.permsErr = PermissionStore(out), err
		},
		func() {
			a.dataDir = filepath.Join(d.cfg.Home, ".var", "app", a.id)
			info, err := os.Stat(a.dataDir)
			if errors.Is(err, fs.ErrNotExist) {
				return
			}
			if err != nil || !info.IsDir() {
				a.dataErr = err
				return
			}
			a.hasData = true
			ctx, cancel := withTimeout(d.ctx, d.cfg.DuTimeout)
			defer cancel()
			a.dataSize, a.dataBig = dirSize(ctx, a.dataDir)
		},
	)
	parallel(fns...)
	a.checked = d.flatpak
	for _, ai := range found {
		if ai != nil {
			a.insts = append(a.insts, ai)
		}
	}
	if a.checked && len(a.insts) == 0 {
		d.searchRemotes(a, insts)
	}
}

// appDetails asks for the permissions of an installed app and checks the
// extra-data of its commit and of the one its origin has now, which the
// next update gets.
func (d *doctor) appDetails(id string, ai *appInst) {
	opt := ai.in.inst.Option()
	ref, origin, commit := ai.info["Ref"], ai.info["Origin"], ai.info["Commit"]
	var installed string
	var remote *summary.Ref
	parallel(
		func() {
			out, err := d.query("info", opt, "--show-permissions", "--", id)
			ai.perms, ai.permErr = strings.TrimRight(out, "\n"), err
		},
		func() {
			out, err := d.query("info", opt, "--show-metadata", "--", id)
			installed, ai.metaErr = out, err
		},
		func() { remote, ai.remoteErr = d.remoteRef(ai.in, origin, ref) },
	)
	switch {
	case remote != nil && remote.Commit == commit:
		ai.checks = append(ai.checks, d.checkCommit(remote.Metadata, ref, "installed and remote commit"))
	default:
		if remote != nil {
			ai.checks = append(ai.checks, d.checkCommit(remote.Metadata, ref, "remote commit, pending update"))
		}
		if ai.metaErr == nil {
			ai.checks = append(ai.checks, d.checkCommit(installed, ref, "installed commit"))
		}
	}
}

// remoteRef looks up ref in the summary of the installation's remote.
func (d *doctor) remoteRef(in *instInfo, remote, ref string) (*summary.Ref, error) {
	if remote == "" || ref == "" {
		return nil, errors.New("flatpak info names no origin")
	}
	var rem *Remote
	for i := range in.remotes {
		if in.remotes[i].Name == remote {
			rem = &in.remotes[i]
		}
	}
	switch {
	case rem == nil:
		return nil, fmt.Errorf("its remote %s is not configured", remote)
	case rem.Has("disabled"):
		return nil, fmt.Errorf("its remote %s is disabled", remote)
	}
	parts := strings.Split(ref, "/")
	if len(parts) != 4 {
		return nil, fmt.Errorf("unexpected ref %s", ref)
	}
	data, err := d.summary(rem.URL, parts[2], remoteSubset(in.inst.Path, remote))
	if err != nil {
		return nil, fmt.Errorf("cannot read the summary of remote %s: %w", remote, err)
	}
	refs, err := summary.Find(data, ref)
	if err != nil {
		return nil, fmt.Errorf("remote %s: %w", remote, err)
	}
	for _, r := range refs {
		if r.Name == ref {
			return &r, nil
		}
	}
	return nil, fmt.Errorf("remote %s no longer has %s", remote, ref)
}

// searchRemotes looks for an app that is not installed in every http(s)
// remote of the installations in use, as "flatpak install" would, for
// every branch.
func (d *doctor) searchRemotes(a *appInfo, insts []*instInfo) {
	type target struct {
		in  *instInfo
		rem Remote
	}
	var targets []target
	seen := map[string]bool{}
	for _, in := range insts {
		for _, rem := range in.remotes {
			if rem.Has("disabled") || seen[rem.URL] ||
				!strings.HasPrefix(rem.URL, "http://") && !strings.HasPrefix(rem.URL, "https://") {
				continue
			}
			seen[rem.URL] = true
			targets = append(targets, target{in, rem})
		}
	}
	finds := make([][]remoteFind, len(targets))
	errs := make([]string, len(targets))
	var fns []func()
	for i, t := range targets {
		fns = append(fns, func() {
			name := t.rem.Name + " (" + t.in.inst.Title() + ")"
			data, err := d.summary(t.rem.URL, d.cfg.Arch, remoteSubset(t.in.inst.Path, t.rem.Name))
			var refs []summary.Ref
			if err == nil {
				refs, err = summary.Find(data, "app/"+a.id+"/"+d.cfg.Arch+"/")
			}
			if err != nil {
				errs[i] = name + ": " + err.Error()
				return
			}
			for _, r := range refs {
				branch := r.Name[strings.LastIndexByte(r.Name, '/')+1:]
				finds[i] = append(finds[i], remoteFind{
					remote: name,
					check:  d.checkCommit(r.Metadata, r.Name, "remote "+name+", branch "+branch),
				})
			}
		})
	}
	parallel(fns...)
	for i := range targets {
		a.found = append(a.found, finds[i]...)
		if errs[i] != "" {
			a.unreadable = append(a.unreadable, errs[i])
		}
	}
}

// checkCommit checks the extra-data of a commit's metadata.
func (d *doctor) checkCommit(meta, ref, label string) commitCheck {
	sources, skipped := metadata.Parse(meta, ref)
	c := commitCheck{label: label, skipped: skipped, extra: make([]ExtraDataCheck, len(sources))}
	base, baseErr := prefetch.UserBaseDir(d.getenv, func() (string, error) { return d.cfg.Home, nil })
	var fns []func()
	for i, src := range sources {
		fns = append(fns, func() {
			e := ExtraDataCheck{Source: src}
			if baseErr == nil {
				p := filepath.Join(prefetch.ExtraDataDir(base, src.Checksum), src.Name)
				if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() && info.Size() == src.Size {
					e.Cached = true
				}
			}
			e.URL = d.checkURL(src.URI)
			c.extra[i] = e
		})
	}
	parallel(fns...)
	return c
}

// dirSize adds up the sizes of the files below dir, symlinks not
// followed, until ctx ends (big is then true).
func dirSize(ctx context.Context, dir string) (size int64, big bool) {
	filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			big = true
			return filepath.SkipAll
		}
		if err != nil || e.IsDir() {
			return nil
		}
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() {
			size += info.Size()
		}
		return nil
	})
	return size, big
}

func (d *doctor) appSection(a *appInfo, insts []*instInfo, ps psInfo) Section {
	s := d.section("App " + a.id)
	r := d.r
	if !a.checked {
		s.add(Warn, "%s: not checked, flatpak is not usable", a.id).hint(r, "see the flatpak line above")
		return s.done()
	}
	if len(a.insts) == 0 {
		s.add(Info, "not installed in any installation")
		for _, u := range a.unreadable {
			s.add(Warn, "cannot look for the app in %s", u).
				hint(r, "check the network; the Network section above says whether the remote can be reached")
		}
		for _, f := range a.found {
			d.commitItems(s, f.check, "flatpak install "+a.id)
		}
		if len(a.found) == 0 {
			s.add(Warn, "%s is not found in any remote", a.id).
				hint(r, "check the spelling of the app id (ids are case-sensitive) and the remotes (\"flatpak remotes\"); FlatPark's apps need the flatpark remote")
		}
	}

	installed := func(runtime string) (where []string, eol string) {
		for _, in := range insts {
			for _, e := range in.runtimes.refs {
				if e.ref == runtime {
					where = append(where, in.inst.Title())
					if reason, ok := e.eol(); ok {
						eol = reason
					}
				}
			}
		}
		return where, eol
	}
	for _, ai := range a.insts {
		s.head("App "+a.id+", "+ai.in.inst.Title(), "%s", ai.in.inst.Title())
		if ai.err != nil {
			s.add(Warn, "cannot read the app: %v", ai.err).hint(r, "flatpak repair %s", ai.in.inst.Option())
			continue
		}
		for _, key := range []string{"Ref", "Version", "Origin", "Commit", "Date"} {
			if v := ai.info[key]; v != "" {
				s.add(Info, "%s: %s", strings.ToLower(key), v)
			}
		}
		if reason := ai.info["End-of-life"]; reason != "" {
			s.add(Warn, "the app is end-of-life: %s", reason).
				hint(r, "it gets no more updates; %s", eolHint(ai.info["End-of-life-rebase"]))
		}
		if rt := ai.info["Runtime"]; rt != "" {
			where, eol := installed(rt)
			switch {
			case len(where) == 0:
				s.add(Fail, "runtime %s is not installed: the app cannot start", rt).
					hint(r, "flatpak install %s; or \"flatpak repair %s\"", rt, ai.in.inst.Option())
			case eol != "":
				s.add(Warn, "runtime %s (%s) is end-of-life: %s", rt, strings.Join(where, ", "), eol).
					hint(r, "the app needs an update to a newer runtime; report it if none comes")
			default:
				s.add(OK, "runtime %s is installed (%s)", rt, strings.Join(where, ", "))
			}
		}

		if ai.permErr != nil {
			s.add(Warn, "cannot read the permissions: %v", ai.permErr).hint(r, "flatpak repair %s", ai.in.inst.Option())
		} else {
			s.add(Info, "permissions (flatpak info --show-permissions):").block(r, ai.perms)
		}

		if ai.remoteErr != nil {
			s.add(Warn, "cannot check the remote's current commit: %v; checking the installed one", ai.remoteErr).
				hint(r, "check the network and \"flatpak remotes %s\"; an app its remote no longer has gets no updates", ai.in.inst.Option())
		}
		if ai.metaErr != nil {
			s.add(Warn, "cannot read the installed commit's metadata: %v", ai.metaErr).hint(r, "flatpak repair %s", ai.in.inst.Option())
		}
		for _, c := range ai.checks {
			d.commitItems(s, c, fmt.Sprintf("flatpak update %s %s", ai.in.inst.Option(), a.id))
		}
	}

	s.head("App "+a.id, "%s", "Data and state")
	for _, in := range insts {
		if !in.exists {
			continue
		}
		dir := filepath.Join(in.inst.Path, "overrides")
		if text, ok := ReadOverride(dir, a.id); ok {
			s.add(Info, "override %s:", filepath.Join(dir, a.id)).block(r, strings.TrimRight(text, "\n"))
		}
		if in.hasGlobal && len(a.insts) > 0 {
			s.add(Info, "also subject to the global override of the %s (see above)", strings.ToLower(in.inst.Title()))
		}
	}
	switch {
	case a.permsErr != nil:
		s.add(Warn, "cannot read the permission store: %v", a.permsErr).
			hint(r, "flatpak permission-show needs the session bus (https://flatpark.org/docs/users/permission-store/)")
	case a.perms == "":
		s.add(Info, "permission store (flatpak permission-show): none")
	default:
		s.add(Info, "permission store (flatpak permission-show):").block(r, a.perms)
	}
	switch {
	case a.dataErr != nil:
		s.add(Warn, "%s: %v", a.dataDir, a.dataErr).hint(r, "check the permissions of %s", a.dataDir)
	case !a.hasData:
		s.add(Info, "%s: does not exist", a.dataDir)
	case a.dataBig:
		s.add(Info, "%s: large (more than %s counted)", a.dataDir, units.Size(a.dataSize))
	default:
		s.add(Info, "%s: %s", a.dataDir, units.Size(a.dataSize))
	}
	switch {
	case ps.err != nil:
		s.add(Info, "running: unknown (%v)", ps.err)
	case len(ps.pids[a.id]) > 0:
		s.add(Info, "running: pid %s", strings.Join(ps.pids[a.id], ", "))
	default:
		s.add(Info, "running: no")
	}
	return s.done()
}

// commitItems are the extra-data checks of a commit; retry is the
// command that downloads them again once the pin is fixed.
func (d *doctor) commitItems(s *builder, c commitCheck, retry string) {
	r := d.r
	if len(c.extra) == 0 && len(c.skipped) == 0 {
		s.add(Info, "no extra-data (%s)", c.label)
		return
	}
	for _, sk := range c.skipped {
		s.add(Info, "extra-data %s: not checked: %s (%s)", sk.Key, sk.Reason, c.label)
	}
	for _, e := range c.extra {
		level, what := e.Judge()
		cached := ""
		if e.Cached {
			cached = " (cached)"
		}
		it := s.add(level, "extra-data %s  %s  %s%s (%s)", e.Source.Name, e.Source.URI, what, cached, c.label)
		if level == Fail {
			it.hint(r, "the file is downloaded from the developer's server; if they replaced or removed it, FlatPark's pin is updated, usually within a day, and then \"%s\" works; report it if that lasts (https://flatpark.org/docs/users/troubleshooting/)", retry)
		}
	}
}

func eolHint(rebase string) string {
	if rebase != "" {
		return "it was renamed to " + rebase + "; install that and uninstall this one"
	}
	return "look for a replacement"
}
