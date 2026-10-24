package main

// Integration tests of rpak clean and of the clean-up after install and
// update, on installations built in temporary directories.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

const (
	commitA = "aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111"
	commitB = "bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222"
)

func init() {
	// The test binary doubles as a process holding a lock: an OFD lock the
	// way flatpak and ostree take them (holdLock), or the flock() another
	// rpak holds on its prefetch directories (holdFlock).
	spec := os.Getenv("RPAK_TEST_HOLD_LOCK")
	if spec == "" {
		return
	}
	var f *os.File
	var err error
	if dir, ok := strings.CutPrefix(spec, "flock:"); ok {
		if f, err = os.Open(dir); err == nil {
			err = unix.Flock(int(f.Fd()), unix.LOCK_SH)
		}
	} else if f, err = os.OpenFile(spec, os.O_RDWR|os.O_CREATE, 0o600); err == nil {
		err = unix.FcntlFlock(f.Fd(), unix.F_OFD_SETLK, &unix.Flock_t{Type: unix.F_RDLCK})
	}
	if err != nil {
		os.Exit(2)
	}
	os.Stdout.WriteString("ready\n")
	io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

// holdLock holds a shared OFD lock on path, as a flatpak pull does on
// repo/.lock and repo-lock, until the returned function is called.
func holdLock(t *testing.T, path string) func() {
	t.Helper()
	return holder(t, path)
}

// holdFlock holds a shared flock() on dir, as a running rpak does on its
// extra-data directory.
func holdFlock(t *testing.T, dir string) func() {
	t.Helper()
	return holder(t, "flock:"+dir)
}

func holder(t *testing.T, spec string) func() {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "RPAK_TEST_HOLD_LOCK="+spec)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if line, _ := bufio.NewReader(stdout).ReadString('\n'); line != "ready\n" {
		t.Fatal("lock holder did not start")
	}
	release := func() { stdin.Close(); cmd.Wait() }
	t.Cleanup(release)
	return release
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "gvariant", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func put(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitmeta(inst, checksum string) string {
	return filepath.Join(inst, "repo", "objects", checksum[:2], checksum[2:]+".commitmeta")
}

// installation builds an installation at inst with commit A (an app with
// extra-data in its detached metadata) and commit B (no ref, extra-data
// too), and returns the path of A's commitmeta.
func installation(t *testing.T, inst string) string {
	t.Helper()
	for _, c := range []string{commitA, commitB} {
		put(t, filepath.Join(inst, "repo", "objects", c[:2], c[2:]+".commit"), []byte("commit"))
		put(t, commitmeta(inst, c), fixture(t, "real.gvariant"))
	}
	put(t, filepath.Join(inst, "repo", "config"), []byte("[core]\nrepo_version=1\nmode=bare-user-only\n"))
	put(t, filepath.Join(inst, "repo", ".lock"), nil) // ostree creates it with the repository
	put(t, filepath.Join(inst, "repo", "refs", "remotes", "flatpark", "app", "org.example.App", "x86_64", "stable"), []byte(commitA+"\n"))
	return commitmeta(inst, commitA)
}

// leftovers adds one of each kind of leftover, plus look-alikes that must
// stay, and returns both lists.
func leftovers(t *testing.T, inst string, user bool) (gone, kept []string) {
	t.Helper()
	empty := filepath.Join(inst, "repo", "refs", "remotes", "deleted", "app", "org.example.Gone")
	os.MkdirAll(empty, 0o755)
	orphan := filepath.Join(inst, "repo", "state", strings.Repeat("c", 64)+".commitpartial")
	put(t, orphan, nil)
	partial := filepath.Join(inst, "repo", "state", commitA+".commitpartial")
	put(t, partial, nil)
	branch := filepath.Join(inst, "app", "org.example.App", "x86_64", "stable")
	deploy := filepath.Join(branch, commitA)
	put(t, filepath.Join(deploy, "metadata"), []byte("[Application]\n"))
	os.MkdirAll(filepath.Join(deploy, "files", "empty-mount-point"), 0o755)
	os.Symlink(commitA, filepath.Join(branch, "active"))
	tmpdir := filepath.Join(branch, "."+commitB+"-Xy12Zq")
	put(t, filepath.Join(tmpdir, "files", "big"), make([]byte, 100_000))
	notTmp := filepath.Join(branch, ".not-a-checkout")
	os.MkdirAll(notTmp, 0o755)
	gone = []string{filepath.Join(inst, "repo", "refs", "remotes", "deleted"), orphan, tmpdir}
	kept = []string{partial, filepath.Join(deploy, "files", "empty-mount-point"), filepath.Join(branch, "active"), notTmp,
		filepath.Join(inst, "repo", "refs", "remotes", "flatpark", "app", "org.example.App", "x86_64", "stable")}
	if user {
		ed := filepath.Join(inst, "extra-data")
		marked := filepath.Join(ed, strings.Repeat("d", 64))
		put(t, filepath.Join(marked, ".rpak-prefetch"), nil)
		put(t, filepath.Join(marked, "app.deb.part"), make([]byte, 1000))
		unmarked := filepath.Join(ed, strings.Repeat("e", 64), "app.deb")
		put(t, unmarked, []byte("mine"))
		fork := filepath.Join(ed, strings.Repeat("f", 64))
		put(t, filepath.Join(fork, ".flatpak-prefetch"), nil)
		put(t, filepath.Join(fork, "app.deb"), []byte("fork"))
		gone = append(gone, marked)
		kept = append(kept, unmarked, fork, filepath.Join(fork, "app.deb"))
	}
	return gone, kept
}

// snapshot describes every file and directory below root: type, mode,
// size, mtime and contents.
func snapshot(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		info, _ := d.Info()
		fmt.Fprintf(&b, "%s %v %d %d", path, info.Mode(), info.Size(), info.ModTime().UnixNano())
		if info.Mode().IsRegular() {
			data, _ := os.ReadFile(path)
			fmt.Fprintf(&b, " %x", sha256.Sum256(data))
		} else if info.Mode()&fs.ModeSymlink != 0 {
			target, _ := os.Readlink(path)
			fmt.Fprintf(&b, " -> %s", target)
		}
		b.WriteByte('\n')
		return nil
	})
	return b.String()
}

func (e *env) runOut(argv ...string) (code int, stdout, stderr string) {
	e.t.Helper()
	cmd := e.command(argv...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	cmd.Run()
	return cmd.ProcessState.ExitCode(), out.String(), errOut.String()
}

func TestCleanDryRunChangesNothing(t *testing.T) {
	e := newEnv(t)
	e.home = t.TempDir()
	installation(t, e.base)
	installation(t, e.sys)
	leftovers(t, e.base, true)
	leftovers(t, e.sys, false)
	appData(t, e)
	before := snapshot(t, e.base) + snapshot(t, e.sys) + snapshot(t, e.home)

	code, stdout, stderr := e.runOut("clean", "--dry-run")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
	}
	if after := snapshot(t, e.base) + snapshot(t, e.sys) + snapshot(t, e.home); after != before {
		t.Errorf("dry run changed files:\nbefore\n%s\nafter\n%s", before, after)
	}
	freed := len(fixture(t, "real.gvariant")) - len(fixture(t, "real.stripped.gvariant"))
	for _, want := range []string{
		"System installation (default):\n",
		"User installation:\n",
		fmt.Sprintf("%.1f MB  flatpark:app/org.example.App/x86_64/stable\n", float64(freed)/(1<<20)),
		fmt.Sprintf("  %s\n", commitB[:12]),
		"/repo/refs: 3 empty directories\n",
		strings.Repeat("c", 64) + ".commitpartial\n",
		"." + commitB + "-Xy12Zq\n",
		"extra-data/" + strings.Repeat("d", 64) + "\n",
		"Data of apps that are no longer installed:\n" +
			"0.1 MB  org.example.Gone  ~/.var/app  overrides\n" +
			"0.0 MB  org.example.OnlyOverride  overrides\n" +
			"Kept ",
		"Would free: 11 objects, ",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, stdout)
		}
	}
	if e.read("calls") != "" {
		t.Error("flatpak was run")
	}
}

// appData gives the user data of an app that is gone (listed), of one that
// is installed in the system installation (not listed) and an override of
// another one that is gone; and returns the paths, which must all stay.
func appData(t *testing.T, e *env) []string {
	t.Helper()
	gone := filepath.Join(e.home, ".var", "app", "org.example.Gone", "data")
	put(t, gone, make([]byte, 100_000))
	here := filepath.Join(e.home, ".var", "app", "org.example.App", "data")
	put(t, here, []byte("x"))
	os.MkdirAll(filepath.Join(e.sys, "app", "org.example.App"), 0o755)
	os.Symlink("x86_64/stable", filepath.Join(e.sys, "app", "org.example.App", "current"))
	override := filepath.Join(e.base, "overrides", "org.example.Gone")
	put(t, override, []byte("[Context]\n"))
	only := filepath.Join(e.base, "overrides", "org.example.OnlyOverride")
	put(t, only, []byte("[Context]\n"))
	return []string{gone, here, override, only}
}

func TestClean(t *testing.T) {
	e := newEnv(t)
	e.home = t.TempDir()
	meta := installation(t, e.base)
	gone, kept := leftovers(t, e.base, true)
	kept = append(kept, appData(t, e)...)
	code, stdout, stderr := e.runOut("clean", "--user")
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
	}
	if b, _ := os.ReadFile(meta); !bytes.Equal(b, fixture(t, "real.stripped.gvariant")) {
		t.Error("xa.extra-data was not stripped")
	}
	if b, _ := os.ReadFile(commitmeta(e.base, commitB)); !bytes.Equal(b, fixture(t, "real.stripped.gvariant")) {
		t.Error("xa.extra-data of the commit without ref was not stripped")
	}
	for _, p := range gone {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s still there", p)
		}
	}
	for _, p := range kept {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s removed", p)
		}
	}
	// One installation: no headings. The app data is listed, not counted.
	if strings.Contains(stdout, "installation:") || !strings.Contains(stdout, "Freed: 6 objects") ||
		!strings.Contains(stdout, "org.example.Gone  ~/.var/app  overrides\n") || strings.Contains(stdout, "org.example.App ") {
		t.Errorf("output:\n%s", stdout)
	}
	// The locks are released, repo-lock removed as glnx does.
	if _, err := os.Stat(filepath.Join(e.base, "repo-lock")); err == nil {
		t.Error("repo-lock left behind")
	}

	// Nothing left the second time.
	code, stdout, _ = e.runOut("clean", "-u")
	if code != 0 || !strings.Contains(stdout, "Freed: 0 objects, 0.00 GB") {
		t.Errorf("second run: exit %d\n%s", code, stdout)
	}
}

func TestCleanBusy(t *testing.T) {
	for _, lockFile := range []string{"repo/.lock", "repo-lock"} {
		t.Run(lockFile, func(t *testing.T) {
			e := newEnv(t)
			installation(t, e.base)
			leftovers(t, e.base, false)
			holdLock(t, filepath.Join(e.base, lockFile))
			before := snapshot(t, filepath.Join(e.base, "repo")) + snapshot(t, filepath.Join(e.base, "app"))
			code, stdout, stderr := e.runOut("clean", "--user")
			if code == 0 || !strings.Contains(stderr, "busy with another transaction") {
				t.Errorf("exit %d\n%s%s", code, stdout, stderr)
			}
			if after := snapshot(t, filepath.Join(e.base, "repo")) + snapshot(t, filepath.Join(e.base, "app")); after != before {
				t.Error("files changed while the repository was locked")
			}
		})
	}
}

func TestCleanReadOnlySystem(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	e := newEnv(t)
	meta := installation(t, e.sys)
	os.Chmod(filepath.Join(e.sys, "repo"), 0o555)
	t.Cleanup(func() { os.Chmod(filepath.Join(e.sys, "repo"), 0o755) })
	before := snapshot(t, e.sys)

	code, stdout, stderr := e.runOut("clean")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, `System installation (default): would free 8.3 kB; run "sudo rpak clean --system"`) {
		t.Errorf("output:\n%s", stdout)
	}
	// Not counted in the total of a real run.
	if !strings.Contains(stdout, "Freed: 0 objects") {
		t.Errorf("output:\n%s", stdout)
	}
	if snapshot(t, e.sys) != before {
		t.Error("read-only installation changed")
	}
	if b, _ := os.ReadFile(meta); !bytes.Equal(b, fixture(t, "real.gvariant")) {
		t.Error("commitmeta changed")
	}

	code, _, stderr = e.runOut("clean", "--system")
	if code != 1 || !strings.Contains(stderr, "is not writable; run this command as root") {
		t.Errorf("explicit --system: exit %d\n%s", code, stderr)
	}
}

func TestCleanOtherInstallation(t *testing.T) {
	e := newEnv(t)
	extra := t.TempDir()
	meta := installation(t, extra)
	put(t, filepath.Join(e.conf, "installations.d", "extra.conf"), []byte(fmt.Sprintf("[Installation \"extra\"]\nPath=%s\n", extra)))
	code, stdout, stderr := e.runOut("clean", "--installation", "extra")
	if code != 0 || !strings.Contains(stdout, "Freed: 2 objects") {
		t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
	}
	if b, _ := os.ReadFile(meta); !bytes.Equal(b, fixture(t, "real.stripped.gvariant")) {
		t.Error("not stripped")
	}
	// Included by default, with a heading.
	installation(t, extra)
	code, stdout, _ = e.runOut("clean", "--dry-run")
	if code != 0 || !strings.Contains(stdout, "Installation system (extra):\n") {
		t.Errorf("exit %d\n%s", code, stdout)
	}
	code, _, stderr = e.runOut("clean", "--installation=nope")
	if code != 1 || !strings.Contains(stderr, "could not find installation nope") {
		t.Errorf("unknown installation: exit %d\n%s", code, stderr)
	}
}

func TestCleanUsage(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		argv []string
		code int
		want string
	}{
		{[]string{"clean", "--help"}, 0, "Usage: rpak clean"},
		{[]string{"-v", "clean", "-h"}, 0, "Usage: rpak clean"},
		{[]string{"clean", "--delete-data"}, 2, "unknown option --delete-data"},
		{[]string{"clean", "-x"}, 2, "unknown option -x"},
		{[]string{"clean", "org.example.App"}, 2, "too many arguments"},
	} {
		code, stdout, stderr := e.runOut(c.argv...)
		if code != c.code || !strings.Contains(stdout+stderr, c.want) {
			t.Errorf("%q: exit %d\n%s%s", c.argv, code, stdout, stderr)
		}
	}
	if e.read("calls") != "" {
		t.Error("flatpak was run")
	}
}

func TestReclaimAfterSuccess(t *testing.T) {
	for _, command := range []string{"install", "update"} {
		t.Run(command, func(t *testing.T) {
			e := newEnv(t)
			e.write("remotes-user", "flatpark\n")
			e.write("updates-user", "")
			meta := installation(t, e.base)
			put(t, filepath.Join(e.base, "extra-data", strings.Repeat("d", 64), ".rpak-prefetch"), nil)
			argv := []string{command, "--user", "-y"}
			if command == "install" {
				argv = append(argv, "flatpark", "org.example.App")
			}

			// flatpak fails: nothing is touched.
			e.write("mode", "exit 1")
			out := e.run(argv...)
			if out.code != 1 || strings.Contains(out.stderr, "freed") {
				t.Fatalf("exit %d\n%s", out.code, out.stderr)
			}
			if b, _ := os.ReadFile(meta); !bytes.Equal(b, fixture(t, "real.gvariant")) {
				t.Fatal("stripped after flatpak failed")
			}

			e.write("mode", "exit 0")
			out = e.run(argv...)
			freed := 2 * (len(fixture(t, "real.gvariant")) - len(fixture(t, "real.stripped.gvariant")))
			want := fmt.Sprintf("rpak: freed %.1f MB of cached extra-data\n", float64(freed)/(1<<20))
			if out.code != 0 || !strings.HasSuffix(out.stderr, want) {
				t.Fatalf("exit %d, want %q in\n%s", out.code, want, out.stderr)
			}
			if b, _ := os.ReadFile(meta); !bytes.Equal(b, fixture(t, "real.stripped.gvariant")) {
				t.Error("not stripped after flatpak succeeded")
			}
			// Only a. runs: rpak clean's other clean-ups are left alone.
			if _, err := os.Stat(filepath.Join(e.base, "extra-data", strings.Repeat("d", 64))); err != nil {
				t.Error("a prefetch directory of an earlier run was removed")
			}
			// Nothing to free: nothing said.
			out = e.run(argv...)
			if out.code != 0 || strings.Contains(out.stderr, "freed") {
				t.Errorf("exit %d\n%s", out.code, out.stderr)
			}
		})
	}
}

// TestNoReclaimAfterNoDeploy: what --no-deploy pulled is installed later
// with --no-pull, from the extra-data in the repository; rpak prefetches as
// usual but leaves it there.
func TestNoReclaimAfterNoDeploy(t *testing.T) {
	for _, command := range []string{"install", "update"} {
		t.Run(command, func(t *testing.T) {
			e := newEnv(t)
			e.write("remotes-user", "flatpark\n")
			e.write("updates-user", "app/com.example.One/x86_64/stable\tflatpark\n")
			meta := installation(t, e.base)
			queryRef := "app/com.example.One/x86_64/stable"
			if command == "install" {
				queryRef = "com.example.One"
			}
			e.app("user", "flatpark", "app/com.example.One/x86_64/stable", queryRef, 10_000)
			argv := []string{command, "--user", "-y", "--no-deploy"}
			if command == "install" {
				argv = append(argv, "flatpark", "com.example.One")
			}
			out := e.run(argv...)
			if out.code != 0 || strings.Contains(out.stderr, "freed") {
				t.Fatalf("exit %d\n%s", out.code, out.stderr)
			}
			e.assertArgv(argv)
			if check := e.read("check"); !strings.Contains(check, "ok ") || strings.Contains(check, "MISSING") {
				t.Errorf("not prefetched: %s\n%s", check, out.stderr)
			}
			if b, _ := os.ReadFile(meta); !bytes.Equal(b, fixture(t, "real.gvariant")) {
				t.Error("xa.extra-data stripped after --no-deploy")
			}
		})
	}
}

func TestReclaimBusyKeepsExitCode(t *testing.T) {
	for _, code := range []int{0, 3} {
		e := newEnv(t)
		e.write("updates-user", "")
		meta := installation(t, e.base)
		holdLock(t, filepath.Join(e.base, "repo-lock"))
		e.write("mode", fmt.Sprintf("exit %d", code))
		out := e.run("update", "--user", "-y")
		if out.code != code {
			t.Errorf("exit %d, want flatpak's %d\n%s", out.code, code, out.stderr)
		}
		if code == 0 && !strings.Contains(out.stderr, "the repository is busy") {
			t.Errorf("no warning:\n%s", out.stderr)
		}
		if b, _ := os.ReadFile(meta); !bytes.Equal(b, fixture(t, "real.gvariant")) {
			t.Error("stripped while locked")
		}
	}
}

func TestReclaimReadOnlySystemHint(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	e := newEnv(t)
	e.write("updates-user", "")
	e.write("updates-system", "")
	installation(t, e.sys)
	os.Chmod(filepath.Join(e.sys, "repo"), 0o555)
	t.Cleanup(func() { os.Chmod(filepath.Join(e.sys, "repo"), 0o755) })
	out := e.run("update", "-y")
	freed := 2 * (len(fixture(t, "real.gvariant")) - len(fixture(t, "real.stripped.gvariant")))
	want := fmt.Sprintf("rpak: run \"sudo rpak clean --system\" to free %.1f MB of cached extra-data\n", float64(freed)/(1<<20))
	if out.code != 0 || !strings.HasSuffix(out.stderr, want) {
		t.Errorf("exit %d, want %q in\n%s", out.code, want, out.stderr)
	}
}

func TestReclaimNotInDryRun(t *testing.T) {
	e := newEnv(t)
	e.write("updates-user", "")
	meta := installation(t, e.base)
	cmd := e.command("update", "--user")
	cmd.Env = append(cmd.Env, "RPAK_DRY_RUN=1")
	out, _ := cmd.CombinedOutput()
	if cmd.ProcessState.ExitCode() != 0 || strings.Contains(string(out), "freed") {
		t.Errorf("exit %d\n%s", cmd.ProcessState.ExitCode(), out)
	}
	if b, _ := os.ReadFile(meta); !bytes.Equal(b, fixture(t, "real.gvariant")) {
		t.Error("stripped in a dry run")
	}
}

// TestCleanSkipsPrefetchDirsInUse: rpak clean leaves the prefetch
// directories alone while another rpak uses them.
func TestCleanSkipsPrefetchDirsInUse(t *testing.T) {
	e := newEnv(t)
	marked := filepath.Join(e.base, "extra-data", strings.Repeat("d", 64))
	put(t, filepath.Join(marked, ".rpak-prefetch"), nil)
	holdFlock(t, filepath.Join(e.base, "extra-data"))
	code, stdout, stderr := e.runOut("clean", "--user")
	if code != 0 || !strings.Contains(stderr, "another rpak is using the prefetched files") {
		t.Errorf("exit %d\n%s%s", code, stdout, stderr)
	}
	if _, err := os.Stat(marked); err != nil {
		t.Error("removed a directory in use")
	}
}
