package main

// Interactive mode: rpak runs on a pseudo-terminal of the test's, as its
// controlling terminal, and the test types the answers.

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// terminal is a pseudo-terminal rpak runs on: the test types into the
// master and collects all output from it.
type terminal struct {
	t      *testing.T
	master *os.File
	slave  *os.File
	name   string
	orig   unix.Termios

	mu  sync.Mutex
	out bytes.Buffer
}

func newTerminal(t *testing.T) *terminal {
	t.Helper()
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Skipf("no pseudo-terminals: %v", err)
	}
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetUint32(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	tm := &terminal{t: t, master: os.NewFile(uintptr(fd), "ptmx"), name: fmt.Sprintf("/dev/pts/%d", n)}
	if tm.slave, err = os.OpenFile(tm.name, os.O_RDWR|unix.O_NOCTTY, 0); err != nil {
		t.Fatal(err)
	}
	if err := unix.IoctlSetWinsize(int(tm.slave.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 30, Col: 100}); err != nil {
		t.Fatal(err)
	}
	orig, err := unix.IoctlGetTermios(int(tm.slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	tm.orig = *orig
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := tm.master.Read(buf)
			tm.mu.Lock()
			tm.out.Write(buf[:n])
			tm.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		tm.slave.Close()
		tm.master.Close()
		<-done
	})
	return tm
}

func (tm *terminal) output() string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return tm.out.String()
}

// settle waits until no more output comes.
func (tm *terminal) settle() {
	for last := -1; ; time.Sleep(100 * time.Millisecond) {
		tm.mu.Lock()
		n := tm.out.Len()
		tm.mu.Unlock()
		if n == last {
			return
		}
		last = n
	}
}

func (tm *terminal) typeIn(s string) {
	tm.t.Helper()
	if _, err := tm.master.WriteString(s); err != nil {
		tm.t.Fatal(err)
	}
}

// mode is the terminal's mode now.
func (tm *terminal) mode() unix.Termios {
	tm.t.Helper()
	t, err := unix.IoctlGetTermios(int(tm.slave.Fd()), unix.TCGETS)
	if err != nil {
		tm.t.Fatal(err)
	}
	return *t
}

// running is rpak running on a terminal.
type running struct {
	t    *testing.T
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

// startOn starts rpak with stdout and stderr on tm, and stdin too unless
// stdin is set; tm is its controlling terminal, as for a shell's job.
func (e *env) startOn(tm *terminal, stdin *os.File, argv ...string) *running {
	e.t.Helper()
	cmd := e.command(argv...)
	cmd.Env = append(cmd.Env, "TERM=xterm", "NO_COLOR=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tm.slave, tm.slave, tm.slave
	ctty := 0
	if stdin != nil {
		cmd.Stdin, ctty = stdin, 1
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: ctty}
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	r := &running{t: e.t, cmd: cmd, done: make(chan struct{})}
	go func() {
		r.err = cmd.Wait()
		close(r.done)
	}()
	e.t.Cleanup(func() {
		select {
		case <-r.done:
		default:
			cmd.Process.Kill()
			<-r.done
		}
	})
	return r
}

// wait returns rpak's exit code, once its output is in.
func (r *running) wait(tm *terminal) int {
	r.t.Helper()
	select {
	case <-r.done:
	case <-time.After(20 * time.Second):
		r.t.Fatalf("rpak did not exit; output:\n%s", tm.output())
	}
	tm.settle()
	var exit *exec.ExitError
	if errors.As(r.err, &exit) {
		return exit.ExitCode()
	}
	if r.err != nil {
		r.t.Fatal(r.err)
	}
	return 0
}

func eventually(t *testing.T, what string, cond func() bool, out func() string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen; output:\n%s", what, out())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (e *env) exists(name string) bool {
	_, err := os.Stat(filepath.Join(e.fake, name))
	return err == nil
}

// slowServer makes the download of the app send 40 kB and then wait for
// release; started is closed when it waits.
func (e *env) slowServer() (started <-chan struct{}, release func()) {
	s, r := make(chan struct{}), make(chan struct{})
	var once sync.Once
	e.setHandler(func(w http.ResponseWriter, req *http.Request) bool {
		content, _ := e.file(req.URL.Path)
		w.Header().Set("Content-Length", fmt.Sprint(len(content)))
		w.Write(content[:40_000])
		w.(http.Flusher).Flush()
		once.Do(func() { close(s) })
		<-r
		w.Write(content[40_000:])
		return true
	})
	var closeOnce sync.Once
	rel := func() { closeOnce.Do(func() { close(r) }) }
	e.t.Cleanup(rel)
	return s, rel
}

const prompt = "[Y/n]: "

// The block rpak shows while it holds an answer has a total line.
const blockTotal = "Total 0/1"

func TestInteractiveHoldsYesUntilPrefetched(t *testing.T) {
	for _, answer := range []string{"", "y", " Yes "} {
		t.Run(fmt.Sprintf("%q", answer), func(t *testing.T) {
			e := newEnv(t)
			e.write("remotes-user", "flatpark\n")
			e.app("user", "flatpark", "app/com.example.One/x86_64/stable", "com.example.One", 100_000)
			e.write("mode", "prompt")
			started, release := e.slowServer()
			tm := newTerminal(t)
			r := e.startOn(tm, nil, "install", "--user", "flatpark", "com.example.One")

			// flatpak runs and asks while the download is under way.
			eventually(t, "the question", func() bool { return strings.Contains(tm.output(), prompt) }, tm.output)
			select {
			case <-started:
			case <-time.After(10 * time.Second):
				t.Fatal("download did not start")
			}
			if got := strings.TrimSpace(e.read("tty")); got == tm.name || !strings.HasPrefix(got, "/dev/pts/") {
				t.Errorf("flatpak's stdin is %q, want a pseudo-terminal of rpak's (the terminal is %s)", got, tm.name)
			}
			before := tm.output()
			if strings.Contains(before, "prefetching") || strings.Contains(before, "rpak:") {
				t.Errorf("rpak wrote while flatpak had the terminal:\n%s", before)
			}

			tm.typeIn(answer + "\r")
			eventually(t, "the progress block", func() bool { return strings.Contains(tm.output(), blockTotal) }, tm.output)
			time.Sleep(300 * time.Millisecond)
			if e.exists("answer") {
				t.Fatalf("flatpak got the answer before the prefetch was done: %q", e.read("answer"))
			}

			release()
			if code := r.wait(tm); code != 0 {
				t.Fatalf("exit %d\n%s", code, tm.output())
			}
			if got := e.read("answer"); got != answer+"\n" {
				t.Errorf("flatpak got %q, want %q", got, answer+"\n")
			}
			if check := e.read("check"); !strings.HasPrefix(check, "ok ") {
				t.Errorf("files when flatpak got the answer: %q", check)
			}
			if dirs := extraDataDirs(t, e.base); len(dirs) != 0 {
				t.Errorf("left behind: %v", dirs)
			}
			out := tm.output()
			// The block is erased before flatpak goes on, and the summary
			// comes after flatpak's output.
			block, erase, complete, summary := strings.LastIndex(out, blockTotal), strings.LastIndex(out, "\x1b[J"), strings.Index(out, "Changes complete."), strings.Index(out, "rpak: prefetch done: 1 ready (1 downloaded")
			if block < 0 || erase < block || complete < erase || summary < complete {
				t.Errorf("block %d, erased %d, flatpak's output %d, summary %d:\n%q", block, erase, complete, summary, out)
			}
			if tm.mode() != tm.orig {
				t.Error("the terminal's mode changed")
			}
		})
	}
}

func TestInteractiveNoIsPassedOnAtOnce(t *testing.T) {
	e := newEnv(t)
	e.write("remotes-user", "flatpark\n")
	path := e.app("user", "flatpark", "app/com.example.One/x86_64/stable", "com.example.One", 100_000)
	e.write("mode", "prompt")
	started, _ := e.slowServer()
	tm := newTerminal(t)
	r := e.startOn(tm, nil, "install", "--user", "flatpark", "com.example.One")
	eventually(t, "the question", func() bool { return strings.Contains(tm.output(), prompt) }, tm.output)
	<-started

	tm.typeIn("N\r")
	// The download is still waiting for the server.
	if code := r.wait(tm); code != 1 {
		t.Fatalf("exit %d, want flatpak's 1\n%s", code, tm.output())
	}
	if got := e.read("answer"); got != "N\n" {
		t.Errorf("flatpak got %q", got)
	}
	if info, err := os.Stat(path + ".part"); err != nil || info.Size() != 40_000 {
		t.Errorf(".part not kept: %v %v", info, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("incomplete file under the real name: %v", err)
	}
	if out := tm.output(); strings.Contains(out, blockTotal) || strings.Contains(out, "rpak:") {
		t.Errorf("rpak wrote something:\n%s", out)
	}
}

func TestInteractiveAnswerAfterPrefetch(t *testing.T) {
	e := newEnv(t)
	e.write("remotes-user", "flatpark\n")
	path := e.app("user", "flatpark", "app/com.example.One/x86_64/stable", "com.example.One", 100_000)
	e.write("mode", "prompt")
	tm := newTerminal(t)
	r := e.startOn(tm, nil, "install", "--user", "flatpark", "com.example.One")
	eventually(t, "the question", func() bool { return strings.Contains(tm.output(), prompt) }, tm.output)
	eventually(t, "the download", func() bool { _, err := os.Stat(path); return err == nil }, tm.output)
	time.Sleep(200 * time.Millisecond)

	tm.typeIn("y\r")
	if code := r.wait(tm); code != 0 {
		t.Fatalf("exit %d\n%s", code, tm.output())
	}
	if got, check := e.read("answer"), e.read("check"); got != "y\n" || !strings.HasPrefix(check, "ok ") {
		t.Errorf("flatpak got %q, files %q", got, check)
	}
	out := tm.output()
	if strings.Contains(out, blockTotal) || strings.Contains(out, "\x1b[") {
		t.Errorf("rpak drew on the terminal:\n%q", out)
	}
	if i, j := strings.Index(out, "Changes complete."), strings.Index(out, "rpak: prefetch done: 1 ready"); i < 0 || j < i {
		t.Errorf("no summary after flatpak's output:\n%s", out)
	}
	if dirs := extraDataDirs(t, e.base); len(dirs) != 0 {
		t.Errorf("left behind: %v", dirs)
	}
}

// sequential checks that rpak prefetched before running flatpak, and gave
// flatpak its own stdin.
func (e *env) sequential(tm *terminal, wantTTY string) {
	e.t.Helper()
	out := tm.output()
	if !strings.Contains(out, "prefetching 1 extra-data file(s)") || !strings.Contains(out, "prefetch done: 1 ready") {
		e.t.Errorf("no plan and summary:\n%s", out)
	}
	if check := e.read("check"); !strings.HasPrefix(check, "ok ") {
		e.t.Errorf("files not in place when flatpak started: %q", check)
	}
	if got := strings.TrimSpace(e.read("tty")); got != wantTTY {
		e.t.Errorf("flatpak's stdin is %q, want %q", got, wantTTY)
	}
}

func TestInteractiveNotWithAssumeYes(t *testing.T) {
	for _, opt := range []string{"-y", "-uy", "--assumeyes", "--noninteractive"} {
		e := newEnv(t)
		e.write("remotes-user", "flatpark\n")
		e.app("user", "flatpark", "app/com.example.One/x86_64/stable", "com.example.One", 10_000)
		tm := newTerminal(t)
		r := e.startOn(tm, nil, "install", "--user", opt, "flatpark", "com.example.One")
		if code := r.wait(tm); code != 0 {
			t.Fatalf("%s: exit %d\n%s", opt, code, tm.output())
		}
		e.sequential(tm, tm.name)
	}
}

func TestInteractiveNotWithoutTerminalStdin(t *testing.T) {
	e := newEnv(t)
	e.write("remotes-user", "flatpark\n")
	e.app("user", "flatpark", "app/com.example.One/x86_64/stable", "com.example.One", 10_000)
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	tm := newTerminal(t)
	r := e.startOn(tm, devNull, "install", "--user", "flatpark", "com.example.One")
	if code := r.wait(tm); code != 0 {
		t.Fatalf("exit %d\n%s", code, tm.output())
	}
	e.sequential(tm, "not a tty")
}

func TestInteractiveNotWithAuthenticator(t *testing.T) {
	e := newEnv(t)
	e.write("remotes-user", "flatpark\n")
	e.app("user", "flatpark", "app/com.example.One/x86_64/stable", "com.example.One", 10_000)
	os.MkdirAll(filepath.Join(e.base, "repo"), 0o755)
	config := "[core]\nrepo_version=1\n\n[remote \"paid\"]\nurl=https://example.com/repo/\nxa.authenticator-name=com.example.Authenticator\n"
	if err := os.WriteFile(filepath.Join(e.base, "repo", "config"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	tm := newTerminal(t)
	r := e.startOn(tm, nil, "install", "--user", "flatpark", "com.example.One")
	if code := r.wait(tm); code != 0 {
		t.Fatalf("exit %d\n%s", code, tm.output())
	}
	e.sequential(tm, tm.name)
}

// TestInteractiveRawMode: while flatpak has its stdin out of canonical
// mode, the terminal is too, and bytes without a newline reach flatpak;
// the terminal's mode is restored after.
func TestInteractiveRawMode(t *testing.T) {
	e := newEnv(t)
	e.write("remotes-user", "flatpark\n")
	path := e.app("user", "flatpark", "app/com.example.One/x86_64/stable", "com.example.One", 10_000)
	e.write("mode", "prompt raw")
	tm := newTerminal(t)
	r := e.startOn(tm, nil, "install", "--user", "flatpark", "com.example.One")
	eventually(t, "the question", func() bool { return strings.Contains(tm.output(), prompt) }, tm.output)
	eventually(t, "the download", func() bool { _, err := os.Stat(path); return err == nil }, tm.output)
	tm.typeIn("\r")
	eventually(t, "raw mode", func() bool { return e.exists("raw") }, tm.output)
	eventually(t, "the terminal out of canonical mode", func() bool { return tm.mode().Lflag&unix.ICANON == 0 }, tm.output)
	tm.typeIn("\x1b[5;1R")
	if code := r.wait(tm); code != 0 {
		t.Fatalf("exit %d\n%s", code, tm.output())
	}
	if got := e.read("rawin"); got != "\x1b[5;1R" {
		t.Errorf("flatpak read %q", got)
	}
	if tm.mode() != tm.orig {
		t.Error("the terminal's mode was not restored")
	}
}

// TestInteractivePolkitAgent: what someone else reads from the terminal
// after changing its mode, as polkit's text agent does, is not taken.
func TestInteractivePolkitAgent(t *testing.T) {
	e := newEnv(t)
	e.write("remotes-user", "flatpark\n")
	path := e.app("user", "flatpark", "app/com.example.One/x86_64/stable", "com.example.One", 10_000)
	e.write("mode", "prompt polkit")
	tm := newTerminal(t)
	r := e.startOn(tm, nil, "install", "--user", "flatpark", "com.example.One")
	eventually(t, "the question", func() bool { return strings.Contains(tm.output(), prompt) }, tm.output)
	eventually(t, "the download", func() bool { _, err := os.Stat(path); return err == nil }, tm.output)
	tm.typeIn("y\r")
	eventually(t, "the password prompt", func() bool { return e.exists("pwprompt") }, tm.output)
	tm.typeIn("secret\r")
	if code := r.wait(tm); code != 0 {
		t.Fatalf("exit %d\n%s", code, tm.output())
	}
	if got := e.read("pw"); got != "secret\n" {
		t.Errorf("the agent read %q", got)
	}
	if strings.Contains(tm.output(), "secret") {
		t.Error("the password was echoed")
	}
	if tm.mode() != tm.orig {
		t.Error("the terminal's mode was not restored")
	}
}

// TestInteractiveInterrupt: Ctrl-C while rpak holds the answer reaches
// rpak and flatpak; the block is erased and the partial download kept.
func TestInteractiveInterrupt(t *testing.T) {
	e := newEnv(t)
	e.write("remotes-user", "flatpark\n")
	path := e.app("user", "flatpark", "app/com.example.One/x86_64/stable", "com.example.One", 100_000)
	e.write("mode", "prompt")
	started, _ := e.slowServer()
	tm := newTerminal(t)
	r := e.startOn(tm, nil, "install", "--user", "flatpark", "com.example.One")
	eventually(t, "the question", func() bool { return strings.Contains(tm.output(), prompt) }, tm.output)
	<-started
	tm.typeIn("\r")
	eventually(t, "the progress block", func() bool { return strings.Contains(tm.output(), blockTotal) }, tm.output)
	tm.typeIn("\x03")
	// The shell running the fake flatpak dies of SIGINT.
	if code := r.wait(tm); code != 130 {
		t.Fatalf("exit %d, want 130\n%s", code, tm.output())
	}
	if e.exists("answer") {
		t.Errorf("flatpak got an answer: %q", e.read("answer"))
	}
	out := tm.output()
	if block, erase := strings.LastIndex(out, blockTotal), strings.LastIndex(out, "\x1b[J"); erase < block {
		t.Errorf("block not erased:\n%q", out)
	}
	if strings.Contains(out, "prefetch done") {
		t.Errorf("summary after an interrupted prefetch:\n%s", out)
	}
	if info, err := os.Stat(path + ".part"); err != nil || info.Size() != 40_000 {
		t.Errorf(".part not kept: %v %v", info, err)
	}
	if tm.mode() != tm.orig {
		t.Error("the terminal's mode changed")
	}
}

// TestInteractiveEndOfInput: ^D at the question is the end of flatpak's
// input too, at once.
func TestInteractiveEndOfInput(t *testing.T) {
	e := newEnv(t)
	e.write("remotes-user", "flatpark\n")
	e.app("user", "flatpark", "app/com.example.One/x86_64/stable", "com.example.One", 100_000)
	e.write("mode", "prompt")
	started, _ := e.slowServer()
	tm := newTerminal(t)
	r := e.startOn(tm, nil, "install", "--user", "flatpark", "com.example.One")
	eventually(t, "the question", func() bool { return strings.Contains(tm.output(), prompt) }, tm.output)
	<-started
	tm.typeIn("\x04")
	if code := r.wait(tm); code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, tm.output())
	}
	if got := e.read("answer"); got != "<EOF>\n" {
		t.Errorf("flatpak read %q", got)
	}
}
