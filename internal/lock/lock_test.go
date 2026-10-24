package lock

import (
	"bufio"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// TestMain doubles as a lock holder: with LOCK_HELPER=KIND it locks the
// file named by LOCK_PATH, says "ready" and holds the lock until stdin is
// closed. KIND is how C code (or a script) would lock it.
func TestMain(m *testing.M) {
	if kind := os.Getenv("LOCK_HELPER"); kind != "" {
		os.Exit(holdLock(kind, os.Getenv("LOCK_PATH")))
	}
	os.Exit(m.Run())
}

func holdLock(kind, path string) int {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return 2
	}
	fd := f.Fd()
	switch kind {
	case "ofd-write", "ofd-read":
		typ := int16(unix.F_WRLCK)
		if kind == "ofd-read" {
			typ = unix.F_RDLCK
		}
		err = unix.FcntlFlock(fd, unix.F_OFD_SETLK, &unix.Flock_t{Type: typ})
	case "posix-write": // what python's fcntl.lockf() takes
		err = unix.FcntlFlock(fd, unix.F_SETLK, &unix.Flock_t{Type: unix.F_WRLCK})
	case "flock":
		err = unix.Flock(int(fd), unix.LOCK_EX)
	default:
		return 3
	}
	if err != nil {
		return 4
	}
	os.Stdout.WriteString("ready\n")
	io.Copy(io.Discard, os.Stdin)
	return 0
}

// holder starts a helper process holding a lock of the given kind on path
// and returns a function that makes it let go.
func holder(t *testing.T, kind, path string) func() {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "LOCK_HELPER="+kind, "LOCK_PATH="+path)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if line, _ := bufio.NewReader(stdout).ReadString('\n'); line != "ready\n" {
		cmd.Wait()
		t.Fatalf("%s helper did not get the lock: exit %d", kind, cmd.ProcessState.ExitCode())
	}
	done := false
	release := func() {
		if !done {
			done = true
			stdin.Close()
			cmd.Wait()
		}
	}
	t.Cleanup(release)
	return release
}

func TestOstreeRepoLockExcludesOtherHolders(t *testing.T) {
	for _, kind := range []string{"ofd-write", "ofd-read", "posix-write"} {
		t.Run(kind, func(t *testing.T) {
			repo := t.TempDir()
			release := holder(t, kind, filepath.Join(repo, ".lock"))
			if l, err := OstreeRepo(repo); !errors.Is(err, ErrBusy) {
				l.Release()
				t.Fatalf("got the lock while another process holds it: %v", err)
			}
			release()
			l, err := OstreeRepo(repo)
			if err != nil {
				t.Fatalf("after release: %v", err)
			}
			l.Release()
			if _, err := os.Stat(filepath.Join(repo, ".lock")); err != nil {
				t.Errorf("ostree's lock file must stay: %v", err)
			}
		})
	}
}

func TestGlnxLockExcludesOtherHolders(t *testing.T) {
	for _, kind := range []string{"ofd-write", "ofd-read", "posix-write"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "repo-lock")
			release := holder(t, kind, path)
			if l, err := Glnx(path); !errors.Is(err, ErrBusy) {
				l.Release()
				t.Fatalf("got the lock while another process holds it: %v", err)
			}
			release()
			l, err := Glnx(path)
			if err != nil {
				t.Fatalf("after release: %v", err)
			}
			l.Release()
			// glnx_release_lock_file() removes the file of an exclusive lock.
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("lock file still there: %v", err)
			}
		})
	}
}

// TestLocksAreNotFlock shows the difference that matters: a flock() holder
// does not keep rpak out, because rpak, like libglnx and ostree, uses
// fcntl OFD locks, which flock() does not see. Conversely the helper's
// flock() would succeed while rpak holds the lock.
func TestLocksAreNotFlock(t *testing.T) {
	repo := t.TempDir()
	holder(t, "flock", filepath.Join(repo, ".lock"))
	l, err := OstreeRepo(repo)
	if err != nil {
		t.Fatalf("flock() holder blocked an OFD lock: %v", err)
	}
	l.Release()
}

// TestHeldLockExcludesOthers checks the other direction: while rpak holds
// the locks, a process taking them like flatpak or ostree would fails.
func TestHeldLockExcludesOthers(t *testing.T) {
	dir := t.TempDir()
	repoLock, err := OstreeRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer repoLock.Release()
	deployLock, err := Glnx(filepath.Join(dir, "repo-lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer deployLock.Release()
	for _, path := range []string{filepath.Join(dir, ".lock"), filepath.Join(dir, "repo-lock")} {
		for _, kind := range []string{"ofd-read", "posix-write"} {
			cmd := exec.Command(os.Args[0], "-test.run=^$")
			cmd.Env = append(os.Environ(), "LOCK_HELPER="+kind, "LOCK_PATH="+path)
			if err := cmd.Run(); cmd.ProcessState.ExitCode() != 4 {
				t.Errorf("%s %s: locked while rpak holds it (%v)", kind, path, err)
			}
		}
	}
	// Another open file description in this process is excluded too.
	if _, err := Glnx(filepath.Join(dir, "repo-lock")); !errors.Is(err, ErrBusy) {
		t.Errorf("second Glnx: %v", err)
	}
}

func TestOpenErrors(t *testing.T) {
	if _, err := OstreeRepo(filepath.Join(t.TempDir(), "missing")); err == nil || errors.Is(err, ErrBusy) {
		t.Errorf("missing repo: %v", err)
	}
	dir := t.TempDir()
	os.Symlink("elsewhere", filepath.Join(dir, "repo-lock"))
	if _, err := Glnx(filepath.Join(dir, "repo-lock")); err == nil || errors.Is(err, ErrBusy) {
		t.Errorf("symlinked lock file (O_NOFOLLOW): %v", err)
	}
}
