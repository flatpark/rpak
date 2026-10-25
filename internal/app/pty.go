package app

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// openPTY opens a new pseudo-terminal. The master is non-blocking, for
// Go's poller, so that closing it ends a Read or Write in progress. The
// slave is blocking: it becomes a process's stdin, and the O_NONBLOCK flag
// would go with it.
func openPTY() (master, slave *os.File, err error) {
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	fail := func(err error) (*os.File, *os.File, error) {
		unix.Close(fd)
		return nil, nil, err
	}
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		return fail(fmt.Errorf("unlocking the pseudo-terminal: %w", err))
	}
	n, err := unix.IoctlGetUint32(fd, unix.TIOCGPTN)
	if err != nil {
		return fail(fmt.Errorf("getting the pseudo-terminal's number: %w", err))
	}
	name := fmt.Sprintf("/dev/pts/%d", n)
	sfd, err := unix.Open(name, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fail(err)
	}
	return os.NewFile(uintptr(fd), "/dev/ptmx"), os.NewFile(uintptr(sfd), name), nil
}

func getTermios(fd int) (*unix.Termios, error) {
	return unix.IoctlGetTermios(fd, unix.TCGETS)
}

// setTermios changes the mode at once (TCSANOW), without dropping input.
func setTermios(fd int, t *unix.Termios) error {
	return unix.IoctlSetTermios(fd, unix.TCSETS, t)
}
