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
	fr "github.com/flatpark/rpak/internal/forceruntime"
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

func (d *doctor) installationsSection(insts []*instInfo) Section {
	s := d.section("Installations")
	for _, in := range insts {
		s.head("%s: %s", in.inst.Title(), in.inst.Path)
		if !in.exists {
			if in.statErr != nil && !errors.Is(in.statErr, fs.ErrNotExist) {
				s.add("installation directory: %v", in.statErr)
			} else {
				s.add("does not exist")
			}
			continue
		}
		access := "read-only for this user"
		if in.writable {
			access = "writable"
		}
		if in.freeErr != nil {
			s.add("%s, free space: %v", access, in.freeErr)
		} else {
			s.add("%s, %s free", access, units.Size(int64(in.free)))
		}
		for _, l := range in.locks {
			name := filepath.Base(filepath.Dir(l.Path)) + "/" + filepath.Base(l.Path)
			if filepath.Base(l.Path) == "repo-lock" {
				name = "repo-lock"
			}
			switch {
			case l.Err != nil:
				s.add("%s: %v", name, l.Err)
			case l.Busy && l.PID > 0:
				s.add("%s: locked by pid %d (%s)", name, l.PID, d.commName(l.PID))
			case l.Busy:
				s.add("%s: locked (holder unknown)", name)
			default:
				s.add("%s: not locked", name)
			}
		}

		if errors.Is(in.remotesErr, errNoFlatpak) {
			// Said once, with the flatpak line.
			s.add("remotes and refs: not listed, no flatpak command is run")
		} else {
			d.refsItems(s, in)
		}
		d.overrideItems(s, in)
	}
	return s.done()
}

// refsItems are the remotes and what is installed.
func (d *doctor) refsItems(s *builder, in *instInfo) {
	if in.remotesErr != nil {
		s.add("remotes: %v", in.remotesErr)
	}
	for _, rem := range in.remotes {
		opts := strings.Join(rem.Options, ",")
		if opts != "" {
			opts = ", " + opts
		}
		note := ""
		if in.inst.User && rem.Name == fr.RemoteName {
			note = "  rpak patch's local repository of reexported apps"
		}
		s.add("remote %s  %s  (priority %s%s)%s", rem.Name, RedactURL(rem.URL), rem.Priority, opts, note)
	}

	counts := func(l refList, what string) string {
		if l.err != nil {
			return what + ": " + l.err.Error()
		}
		return fmt.Sprintf("%d %s", len(l.refs), what)
	}
	s.add("%s, %s", counts(in.apps, "apps"), counts(in.runtimes, "runtimes"))

	// Short, and without the reasons, which are long and mostly the same;
	// the app sections give the reasons for the apps checked.
	var eol []string
	seen := map[string]bool{}
	for _, l := range []refList{in.runtimes, in.apps} {
		for _, e := range l.refs {
			short := ShortRef(e.ref)
			if _, ok := e.eol(); ok && !seen[short] {
				seen[short] = true
				eol = append(eol, short)
			}
		}
	}
	if len(eol) > 0 {
		s.add("end-of-life: %s", ShortList(eol))
	}
}

// ShortRef is a ref as id//branch: org.freedesktop.Platform//24.08 for
// org.freedesktop.Platform/x86_64/24.08 (or runtime/...).
func ShortRef(ref string) string {
	parts := strings.Split(ref, "/")
	if len(parts) == 4 {
		parts = parts[1:]
	}
	if len(parts) != 3 {
		return ref
	}
	return parts[0] + "//" + parts[2]
}

// listed is how many entries ShortList names.
const listed = 8

// ShortList joins the first listed entries, and says how many more there
// are: "a, b, ..., and 3 more".
func ShortList(list []string) string {
	text := strings.Join(list[:min(len(list), listed)], ", ")
	if len(list) > listed {
		text += fmt.Sprintf(", and %d more", len(list)-listed)
	}
	return text
}

// overrideItems are the overrides, which flatpak is not needed to read.
func (d *doctor) overrideItems(s *builder, in *instInfo) {
	if in.hasGlobal {
		s.add("global override %s:", filepath.Join(in.inst.Path, "overrides", "global")).
			block(d.r, strings.TrimRight(in.global, "\n"))
	}
	if len(in.overrides) > 0 {
		s.add("apps with an override: %s", strings.Join(in.overrides, ", "))
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
	switch {
	case x.dir == "":
		s.add("flatpak user directory: %v", x.err)
		return s.done()
	case x.err != nil:
		s.add("%s: %v", x.dir, x.err)
	case !x.exists:
		s.add("%s: does not exist", x.dir)
	default:
		s.add("%s: %d entries, %s", x.dir, x.entries, units.Size(x.size))
		if x.parts > 0 {
			s.add("%d partial downloads (.part), %s", x.parts, units.Size(x.partSz))
		}
	}
	switch {
	case !x.cleanDone:
		s.add("\"rpak clean\" can free: not worked out within %s; see \"rpak clean --dry-run\"", seconds(d.cfg.CleanTimeout))
	default:
		s.add("\"rpak clean\" can free %s (see \"rpak clean --dry-run\")", units.Size(x.freeable))
	}
	return s.done()
}
