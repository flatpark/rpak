package clean

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/flatpark/rpak/internal/lock"
)

// TestMain doubles as a running app: with HOLD_REF_LOCK=PATH it takes a
// POSIX read lock on PATH, as bwrap --lock-file does for files/.ref, says
// "ready" and holds it until stdin is closed.
func TestMain(m *testing.M) {
	if path := os.Getenv("HOLD_REF_LOCK"); path != "" {
		f, err := os.Open(path)
		if err != nil {
			os.Exit(2)
		}
		if err := unix.FcntlFlock(f.Fd(), unix.F_SETLK, &unix.Flock_t{Type: unix.F_RDLCK}); err != nil {
			os.Exit(3)
		}
		os.Stdout.WriteString("ready\n")
		io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

const (
	c1 = "1111111111111111111111111111111111111111111111111111111111111111"
	c2 = "2222222222222222222222222222222222222222222222222222222222222222"
	c3 = "3333333333333333333333333333333333333333333333333333333333333333"
	c4 = "4444444444444444444444444444444444444444444444444444444444444444"
	c5 = "5555555555555555555555555555555555555555555555555555555555555555"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "gvariant", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func write(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// recorder collects what a Cleaner reports.
type recorder struct {
	items    []string
	warnings []string
}

func (r *recorder) cleaner(dryRun bool) *Cleaner {
	return &Cleaner{
		DryRun: dryRun,
		Item:   func(size int64, what string) { r.items = append(r.items, fmt.Sprintf("%d %s", size, what)) },
		Warn:   func(format string, a ...any) { r.warnings = append(r.warnings, fmt.Sprintf(format, a...)) },
	}
}

// commit adds a commit with the given detached metadata (nil: none).
func commit(t *testing.T, repo, checksum string, meta []byte, mode os.FileMode) string {
	t.Helper()
	write(t, objectPath(repo, checksum, "commit"), []byte("commit"), 0o644)
	if meta == nil {
		return ""
	}
	path := objectPath(repo, checksum, "commitmeta")
	write(t, path, meta, mode)
	return path
}

func TestStripExtraData(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	real := fixture(t, "real.gvariant")
	mixed := fixture(t, "mixed.gvariant")
	p1 := commit(t, repo, c1, real, 0o644)
	p2 := commit(t, repo, c2, mixed, 0o640)
	p3 := commit(t, repo, c3, fixture(t, "no-extra-data.gvariant"), 0o644)
	commit(t, repo, c4, nil, 0)
	// detached metadata without its commit: not a commit, left alone
	orphan := objectPath(repo, c5, "commitmeta")
	write(t, orphan, real, 0o644)
	// refs: c1 is both a remote app ref and some other ref
	write(t, filepath.Join(repo, "refs", "remotes", "flatpark", "app", "org.example.A", "x86_64", "stable"), []byte(c1+"\n"), 0o644)
	write(t, filepath.Join(repo, "refs", "heads", "deploy"), []byte(c1+"\n"), 0o644)
	write(t, filepath.Join(repo, "refs", "heads", "app", "org.example.A", "x86_64", "stable"), []byte(c1), 0o644)

	for _, dryRun := range []bool{true, false} {
		var r recorder
		c := r.cleaner(dryRun)
		c.StripExtraData(repo)
		want := []string{
			fmt.Sprintf("%d app/org.example.A/x86_64/stable", len(real)-len(fixture(t, "real.stripped.gvariant"))),
			fmt.Sprintf("%d %s", len(mixed)-len(fixture(t, "mixed.stripped.gvariant")), c2[:12]),
		}
		if !equal(r.items, want) || len(r.warnings) != 0 || c.Objects != 2 {
			t.Fatalf("dry run %v: items %q warnings %q, want %q", dryRun, r.items, r.warnings, want)
		}
		if dryRun {
			if b, _ := os.ReadFile(p1); !bytes.Equal(b, real) {
				t.Fatal("dry run changed a file")
			}
		}
	}
	for path, want := range map[string]string{
		p1: "real.stripped.gvariant", p2: "mixed.stripped.gvariant", p3: "no-extra-data.gvariant", orphan: "real.gvariant",
	} {
		if b, _ := os.ReadFile(path); !bytes.Equal(b, fixture(t, want)) {
			t.Errorf("%s is not %s", path, want)
		}
	}
	if info, _ := os.Stat(p2); info.Mode().Perm() != 0o640 {
		t.Errorf("mode %v, want 0640", info.Mode())
	}
	if leftovers, _ := filepath.Glob(filepath.Join(repo, "objects", "*", ".rpak-tmp-*")); len(leftovers) > 0 {
		t.Errorf("temporary files left: %v", leftovers)
	}
	// Nothing left to do the second time.
	var r recorder
	r.cleaner(false).StripExtraData(repo)
	if len(r.items) != 0 {
		t.Errorf("second run: %q", r.items)
	}
}

func TestStripSkipsBadMetadata(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	mixed := fixture(t, "mixed.gvariant")
	bad := map[string][]byte{
		c1: mixed[:len(mixed)-1],
		c2: []byte("garbage"),
		c3: append(bytes.Clone(mixed[:20]), bytes.Repeat([]byte{0xff}, 40)...),
	}
	for checksum, data := range bad {
		commit(t, repo, checksum, data, 0o644)
	}
	commit(t, repo, c4, []byte{}, 0o644) // empty: an empty a{sv}
	var r recorder
	c := r.cleaner(false)
	c.StripExtraData(repo)
	if len(r.items) != 0 || len(r.warnings) != 3 || c.Failed != 0 {
		t.Errorf("items %q warnings %q failed %d", r.items, r.warnings, c.Failed)
	}
	for checksum, data := range bad {
		if b, _ := os.ReadFile(objectPath(repo, checksum, "commitmeta")); !bytes.Equal(b, data) {
			t.Errorf("%s changed", checksum)
		}
	}
}

func TestCommitRefs(t *testing.T) {
	repo := t.TempDir()
	write(t, filepath.Join(repo, "refs", "heads", "zzz"), []byte(c1), 0o644)
	write(t, filepath.Join(repo, "refs", "heads", "runtime", "org.example.R", "x86_64", "1"), []byte(c1), 0o644)
	write(t, filepath.Join(repo, "refs", "heads", "aaa"), []byte(c2), 0o644)
	write(t, filepath.Join(repo, "refs", "remotes", "r", "bbb"), []byte(c2), 0o644)
	write(t, filepath.Join(repo, "refs", "remotes", "r", "app", "x"), []byte(c3+"\n"), 0o644)
	write(t, filepath.Join(repo, "refs", "remotes", "r", "bad name"), []byte(c4), 0o644)
	write(t, filepath.Join(repo, "refs", "mirrors", "org.example.Collection", "app", "y"), []byte(c4), 0o644)
	got := commitRefs(repo)
	want := map[string]string{c1: "runtime/org.example.R/x86_64/1", c2: "aaa", c3: "r:app/x"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestPrefetchDirs(t *testing.T) {
	base := t.TempDir()
	ed := filepath.Join(base, "extra-data")
	marked := filepath.Join(ed, c1)
	write(t, filepath.Join(marked, ".rpak-prefetch"), []byte("rpak\n"), 0o644)
	write(t, filepath.Join(marked, "app.deb"), make([]byte, 10000), 0o644)
	partial := filepath.Join(ed, c2)
	write(t, filepath.Join(partial, ".rpak-prefetch"), nil, 0o644)
	write(t, filepath.Join(partial, "app.deb.part"), make([]byte, 100), 0o644)
	tmp := filepath.Join(ed, ".rpak-tmp-123")
	write(t, filepath.Join(tmp, ".rpak-prefetch"), nil, 0o644)
	keep := map[string]string{
		filepath.Join(ed, c3, "app.deb"):                       "no marker",
		filepath.Join(ed, c4, ".flatpak-prefetch"):             "the fork's marker",
		filepath.Join(ed, c4, "app.deb.part"):                  "the fork's download",
		filepath.Join(ed, c5, "app.deb.part"):                  "no marker, partial",
		filepath.Join(ed, "loose.part"):                        "not a directory",
		filepath.Join(ed, "nested", c1, ".rpak-prefetch"):      "not directly in extra-data",
		filepath.Join(ed, "linked", "app.deb"):                 "marker is a symlink",
		filepath.Join(ed, "dir-marker", ".rpak-prefetch", "x"): "marker is a directory",
	}
	for path := range keep {
		write(t, path, []byte("x"), 0o644)
	}
	os.Symlink(filepath.Join(marked, ".rpak-prefetch"), filepath.Join(ed, "linked", ".rpak-prefetch"))

	var r recorder
	r.cleaner(true).PrefetchDirs(base)
	if len(r.items) != 3 || !exists(marked) {
		t.Fatalf("dry run: %q", r.items)
	}
	r = recorder{}
	c := r.cleaner(false)
	c.PrefetchDirs(base)
	if len(r.items) != 3 || c.Freed < 10000 {
		t.Errorf("items %q, freed %d", r.items, c.Freed)
	}
	for _, path := range []string{marked, partial, tmp} {
		if exists(path) {
			t.Errorf("%s still there", path)
		}
	}
	for path, why := range keep {
		if !exists(path) {
			t.Errorf("%s (%s) was removed", path, why)
		}
	}
}

func TestRepoLeftovers(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	gone := filepath.Join(repo, "refs", "remotes", "deleted-remote", "app", "org.example.Gone", "x86_64")
	mkdir(t, gone)
	mkdir(t, filepath.Join(repo, "refs", "heads", "deploy", "app", "org.example.Gone"))
	mkdir(t, filepath.Join(repo, "refs", "mirrors"))
	live := filepath.Join(repo, "refs", "remotes", "flathub", "app", "org.example.Live", "x86_64", "stable")
	write(t, live, []byte(c1), 0o644)
	mkdir(t, filepath.Join(repo, "refs", "remotes", "flathub", "app", "org.example.Empty"))
	commit(t, repo, c1, nil, 0)
	state := filepath.Join(repo, "state")
	orphan := filepath.Join(state, c2+".commitpartial")
	partial := filepath.Join(state, c1+".commitpartial") // commit still there: a pull in progress
	invalid := filepath.Join(state, "ABC.commitpartial")
	other := filepath.Join(state, c3+".something")
	for _, p := range []string{orphan, partial, invalid, other} {
		write(t, p, nil, 0o644)
	}

	var r recorder
	r.cleaner(true).RepoLeftovers(repo)
	wantItems := []string{"0 " + filepath.Join(repo, "refs") + ": 8 empty directories", "0 " + orphan}
	if !equal(r.items, wantItems) || !exists(gone) || !exists(orphan) {
		t.Fatalf("dry run: %q", r.items)
	}
	r = recorder{}
	r.cleaner(false).RepoLeftovers(repo)
	if !equal(r.items, wantItems) {
		t.Errorf("items %q, want %q", r.items, wantItems)
	}
	for _, p := range []string{filepath.Join(repo, "refs", "remotes", "deleted-remote"), filepath.Join(repo, "refs", "heads", "deploy"),
		filepath.Join(repo, "refs", "remotes", "flathub", "app", "org.example.Empty"), orphan} {
		if exists(p) {
			t.Errorf("%s still there", p)
		}
	}
	for _, p := range []string{filepath.Join(repo, "refs", "heads"), filepath.Join(repo, "refs", "mirrors"), live, partial, invalid, other} {
		if !exists(p) {
			t.Errorf("%s removed", p)
		}
	}
}

func TestInterruptedDeploys(t *testing.T) {
	inst := t.TempDir()
	tmpName := "." + c1 + "-AbC123"
	// A real deployment with an interrupted update next to it.
	branch := filepath.Join(inst, "app", "org.example.App", "x86_64", "stable")
	deploy := filepath.Join(branch, c2)
	mkdir(t, filepath.Join(deploy, "files", "lib", "GL", "empty")) // mount point: keep
	write(t, filepath.Join(deploy, "metadata"), []byte("[Application]\n"), 0o644)
	os.Symlink(c2, filepath.Join(branch, "active"))
	os.Symlink("x86_64/stable", filepath.Join(inst, "app", "org.example.App", "current"))
	interrupted := filepath.Join(branch, tmpName)
	write(t, filepath.Join(interrupted, "files", "leftover"), make([]byte, 5000), 0o644)
	hex := strings.Repeat("0123456789abcdef", 4)
	keep := []string{
		filepath.Join(branch, "."+strings.ToUpper(hex)+"-AbC123"), // not lowercase hex
		filepath.Join(branch, "."+hex[:63]+"g-AbC123"),            // not hex
		filepath.Join(branch, "."+c3),                             // no suffix
		filepath.Join(branch, "."+c3+"_AbC123"),                   // not a dash
		filepath.Join(branch, c3+"-AbC123x"),                      // no dot
	}
	for _, p := range keep {
		mkdir(t, p)
	}
	// A file named like a checkout is not a checkout.
	write(t, filepath.Join(branch, "."+c4+"-AbC123"), nil, 0o644)
	// An install interrupted before its first deployment.
	gone := filepath.Join(inst, "app", "org.example.Gone")
	goneTmp := filepath.Join(gone, "x86_64", "stable", "."+c5+"-XyZ789")
	write(t, filepath.Join(goneTmp, "files", "x"), []byte("x"), 0o644)
	empty := filepath.Join(inst, "runtime", "org.example.Empty")
	mkdir(t, filepath.Join(empty, "x86_64", "1"))
	// Too deep to count as an interrupted install: something below BRANCH.
	deep := filepath.Join(inst, "runtime", "org.example.Deep")
	mkdir(t, filepath.Join(deep, "x86_64", "1", c1, "files"))

	var r recorder
	r.cleaner(true).InterruptedDeploys(inst)
	sort.Strings(r.items)
	dryItems := strings.Join(r.items, "\n")
	for _, want := range []string{interrupted, goneTmp, " " + gone, " " + empty} {
		if !strings.Contains(dryItems, want) {
			t.Errorf("dry run does not report %s:\n%s", want, dryItems)
		}
	}
	if len(r.items) != 4 || !exists(interrupted) || !exists(gone) {
		t.Fatalf("dry run: %q", r.items)
	}

	r = recorder{}
	r.cleaner(false).InterruptedDeploys(inst)
	sort.Strings(r.items)
	if strings.Join(r.items, "\n") != dryItems {
		t.Errorf("items differ from the dry run:\n%s\nwant\n%s", strings.Join(r.items, "\n"), dryItems)
	}
	for _, p := range []string{interrupted, gone, empty} {
		if exists(p) {
			t.Errorf("%s still there", p)
		}
	}
	for _, p := range append(keep, filepath.Join(deploy, "files", "lib", "GL", "empty"), filepath.Join(branch, "active"),
		filepath.Join(branch, "."+c4+"-AbC123"), filepath.Join(deep, "x86_64", "1", c1, "files")) {
		if !exists(p) {
			t.Errorf("%s removed", p)
		}
	}
}

func TestRemoved(t *testing.T) {
	inst := t.TempDir()
	removed := filepath.Join(inst, ".removed")
	unused := filepath.Join(removed, "org.example.Old-"+c1)
	write(t, filepath.Join(unused, "files", ".ref"), nil, 0o644)
	write(t, filepath.Join(unused, "files", "bin", "app"), make([]byte, 3000), 0o755)
	running := filepath.Join(removed, "org.example.Running-"+c2)
	write(t, filepath.Join(running, "files", ".ref"), nil, 0o644)
	noRef := filepath.Join(removed, "org.example.NoRef-"+c3)
	mkdir(t, filepath.Join(noRef, "files"))
	linked := filepath.Join(removed, "org.example.Linked-"+c4)
	mkdir(t, linked)
	os.Symlink(filepath.Join(running, "files"), filepath.Join(linked, "files"))
	readOnlyRef := filepath.Join(removed, "org.example.ReadOnly-"+c5)
	write(t, filepath.Join(readOnlyRef, "files", ".ref"), nil, 0o444)
	write(t, filepath.Join(removed, "stray-file"), nil, 0o644)

	// A running instance holds a read lock on its .ref, like bwrap.
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "HOLD_REF_LOCK="+filepath.Join(running, "files", ".ref"))
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { stdin.Close(); cmd.Wait() }()
	if line, _ := bufio.NewReader(stdout).ReadString('\n'); line != "ready\n" {
		t.Fatal("lock holder did not start")
	}

	var r recorder
	r.cleaner(true).Removed(inst)
	if len(r.items) != 1 || !strings.HasSuffix(r.items[0], unused) || !exists(unused) {
		t.Fatalf("dry run: %q", r.items)
	}
	r = recorder{}
	r.cleaner(false).Removed(inst)
	if len(r.items) != 1 || exists(unused) {
		t.Errorf("items %q", r.items)
	}
	writable := os.Geteuid() == 0 // root can open the read-only .ref
	for _, p := range []string{running, noRef, linked, filepath.Join(removed, "stray-file")} {
		if !exists(p) {
			t.Errorf("%s removed", p)
		}
	}
	if !writable && !exists(readOnlyRef) {
		t.Errorf("%s removed", readOnlyRef)
	}

	// Once the app has quit, its deployment goes.
	stdin.Close()
	cmd.Wait()
	r = recorder{}
	r.cleaner(false).Removed(inst)
	if exists(running) {
		t.Errorf("%s kept after the app quit: %q", running, r.items)
	}
}

func TestLockIsExclusive(t *testing.T) {
	inst := t.TempDir()
	mkdir(t, filepath.Join(inst, "repo"))
	release, err := Lock(inst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(inst); !errors.Is(err, lock.ErrBusy) {
		t.Errorf("second Lock: %v", err)
	}
	release()
	// Busy on flatpak's own lock alone too.
	other, err := lock.Glnx(filepath.Join(inst, "repo-lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Release()
	if _, err := Lock(inst); !errors.Is(err, lock.ErrBusy) {
		t.Errorf("Lock while repo-lock is held: %v", err)
	}
	// and the ostree lock was given back on the way out
	if l, err := lock.OstreeRepo(filepath.Join(inst, "repo")); err != nil {
		t.Errorf("ostree lock still held: %v", err)
	} else {
		l.Release()
	}
}

func equal(a, b []string) bool {
	return strings.Join(a, "\n") == strings.Join(b, "\n")
}

func TestUninstalledAppData(t *testing.T) {
	home, user, system := t.TempDir(), t.TempDir(), t.TempDir()
	varApp := filepath.Join(home, ".var", "app")
	write(t, filepath.Join(varApp, "org.example.Gone", "cache", "x"), make([]byte, 20000), 0o644)
	write(t, filepath.Join(user, "overrides", "org.example.Gone"), []byte("[Context]\n"), 0o644)
	write(t, filepath.Join(user, "overrides", "org.example.OnlyOverride"), []byte("[Context]\n"), 0o644)
	mkdir(t, filepath.Join(varApp, "org.example.InUser"))
	mkdir(t, filepath.Join(varApp, "org.example.InSystem"))
	mkdir(t, filepath.Join(varApp, "not-an-id"))
	mkdir(t, filepath.Join(varApp, "org.1digit.App"))
	os.Symlink("x86_64/stable", filepath.Join(mkdirp(t, user, "app", "org.example.InUser"), "current"))
	os.Symlink("x86_64/stable", filepath.Join(mkdirp(t, system, "app", "org.example.InSystem"), "current"))
	// installed without data: not listed either way
	os.Symlink("x86_64/stable", filepath.Join(mkdirp(t, user, "app", "org.example.NoData"), "current"))

	before := treeState(t, home) + treeState(t, user)
	got := UninstalledAppData(home, user, []string{user, system})
	var s []string
	for _, a := range got {
		s = append(s, fmt.Sprintf("%s data=%v override=%v big=%v", a.ID, a.Data, a.Override, a.Size >= 20000))
	}
	want := []string{
		"org.example.Gone data=true override=true big=true",
		"org.example.OnlyOverride data=false override=true big=false",
	}
	if !equal(s, want) {
		t.Errorf("got %q, want %q", s, want)
	}
	if after := treeState(t, home) + treeState(t, user); after != before {
		t.Error("listing app data changed files")
	}
}

func TestRemoveAppData(t *testing.T) {
	home, user, system, outside := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	varApp := filepath.Join(home, ".var", "app")
	overrides := filepath.Join(user, "overrides")
	write(t, filepath.Join(varApp, "org.example.Gone", "data", "x"), []byte("x"), 0o644)
	write(t, filepath.Join(overrides, "org.example.Gone"), []byte("[Context]\n"), 0o644)
	// Symlinks are removed, never followed: at the top, below, and as the
	// override.
	write(t, filepath.Join(outside, "dir", "keep"), []byte("keep"), 0o644)
	write(t, filepath.Join(outside, "file"), []byte("keep"), 0o644)
	os.Symlink(filepath.Join(outside, "dir"), filepath.Join(varApp, "org.example.Gone", "escape"))
	os.Symlink(filepath.Join(outside, "dir"), filepath.Join(varApp, "org.example.Linked"))
	os.Symlink(filepath.Join(outside, "file"), filepath.Join(overrides, "org.example.Linked"))
	os.Symlink("x86_64/stable", filepath.Join(mkdirp(t, system, "app", "org.example.Back"), "current"))
	mkdir(t, filepath.Join(varApp, "org.example.Back"))
	insts := []string{user, system}

	for _, a := range []AppData{
		{ID: "org.example.Gone", Data: true, Override: true},
		{ID: "org.example.Linked", Data: true, Override: true},
		// listed, then removed by someone else: not an error
		{ID: "org.example.Vanished", Data: true, Override: true},
	} {
		if err := RemoveAppData(home, user, insts, a); err != nil {
			t.Errorf("%s: %v", a.ID, err)
		}
	}
	for _, p := range []string{
		filepath.Join(varApp, "org.example.Gone"), filepath.Join(overrides, "org.example.Gone"),
		filepath.Join(varApp, "org.example.Linked"), filepath.Join(overrides, "org.example.Linked"),
	} {
		if exists(p) {
			t.Errorf("%s still there", p)
		}
	}
	for _, p := range []string{filepath.Join(outside, "dir", "keep"), filepath.Join(outside, "file"), varApp, overrides} {
		if !exists(p) {
			t.Errorf("%s removed", p)
		}
	}

	// Refused: installed since it was listed, and names that are not app
	// IDs (which UninstalledAppData never returns).
	for _, a := range []AppData{
		{ID: "org.example.Back", Data: true},
		{ID: "..", Data: true},
		{ID: "", Data: true},
		{ID: "org.example/../../x.y", Data: true},
	} {
		if err := RemoveAppData(home, user, insts, a); err == nil {
			t.Errorf("%q: no error", a.ID)
		}
	}
	if !exists(filepath.Join(varApp, "org.example.Back")) || !exists(home) {
		t.Error("removed what was refused")
	}

	// What cannot be removed is an error, as for flatpak_rm_rf(), and the
	// override stays.
	if os.Geteuid() != 0 {
		ro := filepath.Join(varApp, "org.example.ReadOnly", "ro")
		write(t, filepath.Join(ro, "f"), nil, 0o644)
		write(t, filepath.Join(overrides, "org.example.ReadOnly"), nil, 0o644)
		os.Chmod(ro, 0o555)
		t.Cleanup(func() { os.Chmod(ro, 0o755) })
		if err := RemoveAppData(home, user, insts, AppData{ID: "org.example.ReadOnly", Data: true, Override: true}); err == nil {
			t.Error("read-only directory: no error")
		}
		if !exists(filepath.Join(overrides, "org.example.ReadOnly")) {
			t.Error("override removed although the data is still there")
		}
	}
}

func TestChildPath(t *testing.T) {
	for name, ok := range map[string]bool{"a.b": true, ":1.42": true, "": false, ".": false, "..": false, "a/b": false, "../a": false} {
		if _, err := childPath("/home/me/.var/app", name); (err == nil) != ok {
			t.Errorf("%q: %v", name, err)
		}
	}
}

func TestDBusName(t *testing.T) {
	for name, want := range map[string]bool{
		"org.example.App": true, "org.example.App-1": true, "a.b": true, "_a.b_": true,
		"org.example": true, "com.example.App.Sub": true, ":1.42": true, ":.a": true,
		"": false, "org": false, ".org.a": false, "org.": false, "org..a": false, "org.1a": false,
		"1org.a": false, "org/a.b": false, "org.ü.b": false, ":1": false, strings.Repeat("a.", 128) + "a": false,
	} {
		if got := dbusName(name); got != want {
			t.Errorf("%q: %v, want %v", name, got, want)
		}
	}
}

// TestReadDirOrder: entries come in inode order, as GLib's enumerator
// returns them, whatever readdir() order the file system has.
func TestReadDirOrder(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 50; i++ {
		write(t, filepath.Join(dir, fmt.Sprintf("f%02d", (i*37)%50)), nil, 0o644)
	}
	mkdir(t, filepath.Join(dir, "sub"))
	os.Symlink("f00", filepath.Join(dir, "link"))
	entries, err := readDir(dir)
	if err != nil || len(entries) != 52 {
		t.Fatalf("%d entries, %v", len(entries), err)
	}
	var last uint64
	for _, e := range entries {
		var st unix.Stat_t
		unix.Lstat(filepath.Join(dir, e.Name()), &st)
		if st.Ino < last {
			t.Errorf("%s out of inode order", e.Name())
		}
		last = st.Ino
		info, _ := e.Info()
		if e.Type() != info.Mode().Type() {
			t.Errorf("%s: type %v, lstat says %v", e.Name(), e.Type(), info.Mode().Type())
		}
	}
	if _, err := readDir(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing directory: %v", err)
	}
}

func mkdirp(t *testing.T, parts ...string) string {
	t.Helper()
	path := filepath.Join(parts...)
	mkdir(t, path)
	return path
}

// treeState lists everything below root with mode, size and mtime.
func treeState(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil {
			fmt.Fprintf(&b, "%s %v %d %d\n", path, info.Mode(), info.Size(), info.ModTime().UnixNano())
		}
		return nil
	})
	return b.String()
}
