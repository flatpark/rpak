package app

// Interactive mode: flatpak asks its questions while rpak prefetches.
//
// In sequential mode rpak prefetches, and only then runs flatpak, which
// shows the transaction and asks whether to proceed. In interactive mode
// (see openConsole for when) rpak starts flatpak as soon as the plan is
// made and prefetches in the background without a word: the terminal is
// flatpak's. flatpak's stdout and stderr are the terminal. Its stdin is a
// pseudo-terminal, as flatpak only asks when stdin and stdout are both
// terminals, and rpak passes the user's input on to it, holding back an
// answer that would let flatpak go ahead until the prefetch is done (see
// forward). Meanwhile rpak shows its progress where the cursor is, below
// the answer, and erases it before passing the answer on.
//
// The terminal stays in its mode, so it echoes and edits lines itself,
// except while flatpak has its stdin out of canonical mode, which it does
// for the whole transaction: it then writes cursor position queries to
// stdout and reads the terminal's answers, which have no newline, from
// stdin. rpak follows: the terminal goes out of canonical mode too and
// bytes are passed on as they come. To learn of the mode changes at once,
// the slave has EXTPROC set, and every change of its mode wakes the master,
// which is in packet mode, with TIOCPKT_IOCTL. EXTPROC also leaves the
// line editing to the terminal, which has done it.
//
// polkit's text agent, which flatpak runs when no other one is around,
// reads passwords from /dev/tty, the same terminal, after changing its
// mode. Whenever the terminal is not in the mode rpak left it in, rpak
// does not read it.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/flatpark/rpak/internal/args"
	"github.com/flatpark/rpak/internal/installation"
)

// console connects the terminal to flatpak's stdin.
type console struct {
	in            int // the terminal of stdin, opened again, non-blocking
	orig          unix.Termios
	master, slave *os.File
	wake          [2]int // a pipe; closing wake[1] ends forward
	stop          chan struct{}
	closeMaster   func()
	wg            sync.WaitGroup

	mu       sync.Mutex
	expected unix.Termios // the mode rpak keeps the terminal in
	raw      bool         // following flatpak's stdin out of canonical mode
	changed  bool         // the terminal was put out of canonical mode
}

// interactive reports why the sequential mode is used, or "" if flatpak
// can ask while rpak prefetches: stdin and stdout are terminals, flatpak
// is not told to assume yes, rpak's progress can be erased (stderr is not
// a terminal without cursor movement), and no remote of the installations
// involved uses an authenticator, which may ask for a password: flatpak
// turns the echo of its stdin off for that, which would not stop the
// terminal's own echo.
func interactive(parsed args.Parsed) string {
	switch {
	case parsed.AssumeYes:
		return "-y"
	case !isTerminal(os.Stdin) || !isTerminal(os.Stdout):
		return "not a terminal"
	}
	if t := detectTerm(os.Stderr); t.tty && !t.redraw {
		return "dumb terminal"
	}
	var insts []installation.Installation
	if parsed.ExplicitInstallation {
		var err error
		if insts, _, err = (installation.Env{}).Select(parsed.Installations, false); err != nil {
			return err.Error()
		}
	} else {
		// Without options flatpak may pick any installation that has
		// the remote.
		insts = installation.Env{}.All()
	}
	for _, inst := range insts {
		remotes, err := inst.AuthenticatedRemotes()
		if err != nil {
			return err.Error()
		}
		if len(remotes) > 0 {
			return "authenticator"
		}
	}
	return ""
}

// openConsole prepares the pseudo-terminal for flatpak's stdin.
func openConsole() (*console, error) {
	// A file description of our own: O_NONBLOCK on the shared one would
	// apply to flatpak's stdout and stderr, and to the shell.
	in, err := unix.Open("/proc/self/fd/0", unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	c := &console{in: in, stop: make(chan struct{}), wake: [2]int{-1, -1}}
	fail := func(err error) (*console, error) {
		c.close()
		return nil, err
	}
	orig, err := getTermios(in)
	if err != nil {
		return fail(err)
	}
	c.orig, c.expected = *orig, *orig
	if c.master, c.slave, err = openPTY(); err != nil {
		return fail(err)
	}
	var once sync.Once
	c.closeMaster = func() { once.Do(func() { c.master.Close() }) }
	mode := *orig
	mode.Lflag |= unix.ICANON | unix.EXTPROC
	mode.Lflag &^= unix.ECHO | unix.ECHONL
	if err := setTermios(int(c.slave.Fd()), &mode); err != nil {
		return fail(err)
	}
	// flatpak takes the size from stdout; for anything that asks stdin.
	if ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ); err == nil {
		_ = unix.IoctlSetWinsize(int(c.slave.Fd()), unix.TIOCSWINSZ, ws)
	}
	conn, err := c.master.SyscallConn()
	if err != nil {
		return fail(err)
	}
	if cerr := conn.Control(func(fd uintptr) { err = unix.IoctlSetPointerInt(int(fd), unix.TIOCPKT, 1) }); cerr != nil {
		err = cerr
	}
	if err != nil {
		return fail(err)
	}
	if err := unix.Pipe2(c.wake[:], unix.O_CLOEXEC); err != nil {
		return fail(err)
	}
	return c, nil
}

// close closes what openConsole opened; the terminal must be left alone
// by then (see start).
func (c *console) close() {
	for _, fd := range append([]int{c.in}, c.wake[:]...) {
		if fd >= 0 {
			unix.Close(fd)
		}
	}
	c.in, c.wake = -1, [2]int{-1, -1}
	if c.master != nil {
		c.closeMaster()
		c.slave.Close()
	}
}

// start passes the terminal's input on to flatpak until the returned stop
// is called, which also puts the terminal back in its mode. hold is called
// for an answer that is to wait for the prefetch and returns what to wait
// for, nil if there is no need.
func (c *console) start(hold func() <-chan struct{}) (stop func()) {
	cont := make(chan os.Signal, 1)
	signal.Notify(cont, syscall.SIGCONT)
	c.wg.Add(3)
	go func() {
		defer c.wg.Done()
		c.watch()
	}()
	go func() {
		defer c.wg.Done()
		c.forward(hold)
	}()
	go func() {
		defer c.wg.Done()
		// After a stop (Ctrl-Z), the shell may have changed the mode.
		for range cont {
			c.mu.Lock()
			if c.raw {
				_ = setTermios(c.in, &c.expected)
			}
			c.mu.Unlock()
		}
	}()
	return func() {
		close(c.stop)
		unix.Close(c.wake[1])
		c.wake[1] = -1
		c.closeMaster()
		signal.Stop(cont)
		close(cont)
		c.wg.Wait()
		c.mu.Lock()
		if c.changed {
			_ = setTermios(c.in, &c.orig)
		}
		c.mu.Unlock()
	}
}

// watch reads the master, where nothing but the notices of mode changes
// of flatpak's stdin arrive: ECHO is off and nothing writes to the slave.
func (c *console) watch() {
	buf := make([]byte, 512)
	for {
		n, err := c.master.Read(buf)
		if err != nil {
			return
		}
		if n > 0 && buf[0]&unix.TIOCPKT_IOCTL != 0 {
			c.follow()
		}
	}
}

// follow puts the terminal out of canonical mode while flatpak's stdin is,
// and back into its own mode after.
func (c *console) follow() {
	t, err := getTermios(int(c.slave.Fd()))
	if err != nil {
		return
	}
	raw := t.Lflag&unix.ICANON == 0
	c.mu.Lock()
	defer c.mu.Unlock()
	if raw == c.raw {
		return
	}
	c.raw = raw
	mode := c.orig
	if raw {
		// What flatpak_enable_raw_mode() does, plus ECHONL, which does
		// nothing without ICANON, but which polkit's agent clears.
		mode.Lflag &^= unix.ICANON | unix.ECHO
		mode.Lflag |= unix.ECHONL
		mode.Cc[unix.VMIN], mode.Cc[unix.VTIME] = 1, 0
		c.changed = true
	}
	if setTermios(c.in, &mode) != nil {
		return
	}
	// As the driver keeps it, for ours.
	if t, err := getTermios(c.in); err == nil {
		mode = *t
	}
	c.expected = mode
}

// ours reports whether the terminal is in the mode rpak keeps it in. If
// not, someone else changed it to read from it.
func (c *console) ours() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, err := getTermios(c.in)
	return err != nil || *t == c.expected
}

func (c *console) isRaw() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.raw
}

// yieldInterval is how often forward looks whether the terminal is back
// in its mode while someone else reads it.
const yieldInterval = 20 * time.Millisecond

// forward passes the terminal's input on to flatpak, until stop, or until
// the input ends, when it closes the master, so that flatpak reads the end
// of the file too.
//
// In canonical mode the input comes a line at a time, as the terminal
// edited it. A line is passed on at once once the prefetch is done, and so
// is "n" or "no" (in any case, with spaces around, as flatpak takes it),
// which may answer some other question than the last one: the downloads
// go on until flatpak exits. Any other line, an empty one (yes by default)
// included, is held back while hold's channel is open. Out of canonical
// mode bytes are passed on as they come.
func (c *console) forward(hold func() <-chan struct{}) {
	buf := make([]byte, 4096)
	var pending []byte
	fds := make([]unix.PollFd, 2)
	for {
		select {
		case <-c.stop:
			return
		default:
		}
		if !c.ours() {
			fds[0] = unix.PollFd{Fd: int32(c.wake[0]), Events: unix.POLLIN}
			if _, err := unix.Poll(fds[:1], int(yieldInterval/time.Millisecond)); err != nil && !errors.Is(err, unix.EINTR) {
				return
			}
			continue
		}
		fds[0] = unix.PollFd{Fd: int32(c.in), Events: unix.POLLIN}
		fds[1] = unix.PollFd{Fd: int32(c.wake[0]), Events: unix.POLLIN}
		if _, err := unix.Poll(fds, -1); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return
		}
		if fds[1].Revents != 0 || fds[0].Revents == 0 {
			continue
		}
		// The terminal may have changed hands while we were waiting.
		if !c.ours() {
			continue
		}
		n, err := unix.Read(c.in, buf)
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
			continue
		}
		if n <= 0 || err != nil {
			// ^D at the start of a line, or the terminal is gone.
			c.write(pending)
			c.closeMaster()
			return
		}
		pending = append(pending, buf[:n]...)
		if c.isRaw() {
			c.write(pending)
			pending = nil
			continue
		}
		if !c.lineEnd(pending[len(pending)-1]) {
			continue // ^D in the middle of a line, or a very long one
		}
		line := pending
		pending = nil
		if !answersNo(line) {
			if wait := hold(); wait != nil {
				select {
				case <-wait:
				case <-c.stop:
					return
				}
			}
		}
		c.write(line)
	}
}

// lineEnd reports whether b ends a line in canonical mode.
func (c *console) lineEnd(b byte) bool {
	if b == '\n' {
		return true
	}
	// 0 is _POSIX_VDISABLE.
	return b != 0 && (b == c.orig.Cc[unix.VEOL] || b == c.orig.Cc[unix.VEOL2])
}

func (c *console) write(b []byte) {
	if len(b) > 0 {
		_, _ = c.master.Write(b)
	}
}

// answersNo reports whether line answers "no" to flatpak_yes_no_prompt():
// "n" or "no", in any case, with ASCII white space around it (g_strstrip).
func answersNo(line []byte) bool {
	s := bytes.Trim(line, " \t\n\v\f\r")
	return bytes.EqualFold(s, []byte("n")) || bytes.EqualFold(s, []byte("no"))
}

// warnSequential tells why rpak could not use the interactive mode it
// meant to.
func warnSequential(err error) {
	fmt.Fprintf(os.Stderr, "rpak: warning: cannot pass flatpak's questions on while prefetching, asking after it: %v\n", err)
}
