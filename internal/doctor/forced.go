package doctor

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	fr "github.com/flatpark/rpak/internal/forceruntime"
)

// rpak install/update/patch --force-runtime changes things doctor looks at, on
// purpose: the deployment's metadata no longer is the commit's xa.metadata
// (patch), or the app comes from rpak's unsigned local remote rpak-local
// (reexport). doctor reads rpak's records of the user installation and says
// so wherever that shows, and checks the extra-data of a reexported app
// against its upstream remote. A deployment whose metadata differs from
// its commit without a record is reported as just that.

// forcedInfo is what doctor knows of forced runtimes in the user
// installation.
type forcedInfo struct {
	user     string // the user installation, "" if unknown
	records  []fr.Record
	err      error // reading the records
	hasFile  bool
	repo     bool     // the local repository exists
	fromRepo []string // refs installed with origin rpak-local
	// state is the runtime each record's deployment names now; missing
	// when it is not installed.
	state map[string]deployState
}

type deployState struct {
	origin  string
	runtime string
	err     error
}

func (d *doctor) forced() *forcedInfo {
	f := &forcedInfo{state: map[string]deployState{}}
	user, err := d.installEnv().User()
	if err != nil {
		return f
	}
	f.user = user.Path
	f.hasFile = fr.HasRecords(f.user)
	f.records, f.err = fr.Load(f.user)
	if _, err := os.Stat(fr.RepoPath(f.user)); err == nil {
		f.repo = true
	}
	f.fromRepo = fr.LocalUsers(f.user)
	for _, r := range f.records {
		dep, ok, err := fr.Active(f.user, r.Ref)
		if !ok && err == nil {
			continue
		}
		st := deployState{origin: dep.Origin, err: err}
		if err == nil {
			text, err := dep.MetadataFile()
			if err == nil {
				var p fr.Pref
				p, err = fr.RuntimeOf(text)
				st.runtime = p.String()
			}
			st.err = err
		}
		f.state[r.Ref] = st
	}
	return f
}

// record is the record of ref, if it is forced in the user installation.
func (f *forcedInfo) record(user bool, ref string) (fr.Record, bool) {
	if f == nil || !user {
		return fr.Record{}, false
	}
	return fr.Find(f.records, ref)
}

// shown reports whether the section is worth showing: rpak has forced
// something here, now or before.
func (f *forcedInfo) shown(remote bool) bool {
	return f.hasFile || f.repo || remote || len(f.fromRepo) > 0
}

// forcedSection lists the records, how each deployment stands, and what is
// installed from rpak-local. remote reports whether the user installation
// has the remote rpak-local.
func (d *doctor) forcedSection(f *forcedInfo, remote bool) (Section, bool) {
	if f == nil || !f.shown(remote) {
		return Section{}, false
	}
	s := d.section("Patches")
	switch {
	case f.err != nil:
		s.add("%s: %v", fr.RecordsPath(f.user), f.err)
	case len(f.records) == 0:
		s.add("no patches")
	}
	for _, r := range f.records {
		line := strings.TrimPrefix(r.Ref, "app/") + "  " + fr.Kind + " (method " + r.Method + "): built for " + r.Original +
			", forced to " + r.Runtime + "  (target " + targetOf(r) + ", upstream remote " + r.Remote + ")"
		st, installed := f.state[r.Ref]
		switch {
		case !installed:
			line += "; not installed"
		case st.err != nil:
			line += "; deployment: " + st.err.Error()
		case r.Method == fr.MethodReexport && st.origin != fr.RemoteName:
			line += "; installed from " + st.origin + ", not " + fr.RemoteName + ": not forced now"
		case st.runtime == r.Runtime:
			line += "; deployment runs on " + st.runtime
		case r.Method == fr.MethodPatch:
			line += "; deployment runs on " + st.runtime + ": not forced now (deployed by flatpak itself; rpak update forces it again)"
		default:
			line += "; deployment runs on " + st.runtime
		}
		s.add("%s", line)
	}
	switch {
	case remote:
		s.add("remote %s: %s", fr.RemoteName, fr.RepoPath(f.user))
	case f.repo:
		s.add("remote %s: not configured, the local repository %s is there", fr.RemoteName, fr.RepoPath(f.user))
	}
	if f.repo || remote {
		if _, err := os.Stat(fr.RepoPath(f.user)); errors.Is(err, fs.ErrNotExist) {
			s.add("local repository %s: does not exist (rpak recreates it when it needs it)", fr.RepoPath(f.user))
		}
	}
	if len(f.fromRepo) > 0 {
		s.add("installed from %s: %s", fr.RemoteName, strings.Join(f.fromRepo, ", "))
		if !remote {
			s.add("remote %s: not configured, so these get no updates until rpak update adds it again", fr.RemoteName)
		}
		for _, ref := range f.fromRepo {
			if _, ok := fr.Find(f.records, ref); !ok {
				s.add("%s comes from %s but has no record (rpak patch --reset cannot undo it; reinstall it from its remote)", ref, fr.RemoteName)
			}
		}
	}
	return s.done(), true
}

func targetOf(r fr.Record) string {
	if r.Target == "" {
		return fr.Latest
	}
	return r.Target
}

// metadataDiff compares an installed ref's deployment metadata with its
// commit's xa.metadata, as deploying does. It returns "" when they are the
// same or cannot be compared, else the two runtimes.
func metadataDiff(inst, ref, commit string) (deployed, built string, differs bool) {
	text, err := os.ReadFile(filepath.Join(inst, filepath.FromSlash(ref), "active", "metadata"))
	if err != nil {
		return "", "", false
	}
	orig, err := fr.CommitMetadata(inst, commit)
	if err != nil || orig == string(text) {
		return "", "", false
	}
	a, _ := fr.RuntimeOf(string(text))
	b, _ := fr.RuntimeOf(orig)
	return a.String(), b.String(), true
}

// forcedItems are the lines of an app section about forcing and about the
// deployment's metadata.
func (d *doctor) forcedItems(s *builder, ai *appInst, f *forcedInfo) {
	ref, commit := ai.info["Ref"], ai.info["Commit"]
	r, forced := f.record(ai.in.inst.User, ref)
	if forced {
		s.add("forced runtime: built for %s, forced to %s  (rpak patch %s, method %s, upstream remote %s)", r.Original, r.Runtime, fr.Kind, r.Method, r.Remote)
	}
	if ref == "" || commit == "" {
		return
	}
	deployed, built, differs := metadataDiff(ai.in.inst.Path, ref, commit)
	switch {
	case !differs:
	case forced:
		s.add("deployment's metadata: rewritten by rpak patch %s (runtime %s, the commit's %s)", fr.Kind, deployed, built)
	default:
		s.add("deployment's metadata: differs from its commit's xa.metadata (runtime %s, the commit's %s)", deployed, built)
	}
}

// hasLocalRemote reports whether the user installation has the remote
// rpak-local.
func hasLocalRemote(insts []*instInfo) bool {
	for _, in := range insts {
		if !in.inst.User {
			continue
		}
		for _, rem := range in.remotes {
			if rem.Name == fr.RemoteName {
				return true
			}
		}
	}
	return false
}
