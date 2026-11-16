// Package lock takes the two locks that guard a flatpak installation's
// repository, in a way that excludes flatpak and ostree themselves.
//
// Both are open file description (OFD) locks over the whole file, taken
// with fcntl(F_OFD_SETLK), falling back to flock() only if the kernel
// rejects OFD locks with EINVAL, exactly as the C code does. This matters:
// on Linux, flock() locks and fcntl() locks do not see each other, so a
// flock()-only implementation would not exclude anyone. (OFD and classic
// POSIX record locks do conflict with each other.)
//
//   - ostree's repository lock, repo/.lock: push_repo_lock() and
//     do_repo_lock() in ostree's src/libostree/ostree-repo.c. The file is
//     opened with O_CREAT|O_RDWR|O_CLOEXEC, mode 0660, and never removed.
//     Transactions (pulls) hold it shared, pruning holds it exclusively.
//   - flatpak's <installation>/repo-lock: flatpak_dir_repo_lock() in
//     common/flatpak-dir.c, which is glnx_make_lock_file() of libglnx's
//     glnx-lockfile.c: O_CREAT|O_RDWR|O_NOFOLLOW|O_CLOEXEC|O_NOCTTY, mode
//     0600; once locked, a file that has been unlinked meanwhile is opened
//     again; an exclusive holder unlinks the file when releasing it. Pulls
//     and deploys hold it shared, prune exclusively.
//
// Only non-blocking exclusive locks are needed here, and Probe, which
// asks whether one could be taken without taking it.
package lock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// ErrBusy means that someone else holds the lock.
var ErrBusy = errors.New("the repository is busy with another transaction")

// File is a held lock.
type File struct {
	f      *os.File
	path   string
	unlink bool
}

// Release drops the lock. For a glnx lock file it also removes the file,
// as glnx_release_lock_file() does for an exclusive lock.
func (l *File) Release() {
	if l == nil || l.f == nil {
		return
	}
	if l.unlink {
		_ = os.Remove(l.path)
	}
	_ = l.f.Close()
	l.f = nil
}

// OstreeRepo takes ostree's repository lock of the repository at repoPath
// exclusively, without waiting.
func OstreeRepo(repoPath string) (*File, error) {
	path := filepath.Join(repoPath, ".lock")
	fd, err := openRetry(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC, 0o660)
	if err != nil {
		return nil, fmt.Errorf("opening lock file %s: %w", path, err)
	}
	f := os.NewFile(uintptr(fd), path)
	if err := lockExclusive(fd); err != nil {
		f.Close()
		return nil, lockError(path, err)
	}
	return &File{f: f, path: path}, nil
}

// Glnx takes the lock file at path exclusively, without waiting, the way
// glnx_make_lock_file (AT_FDCWD, path, LOCK_EX | LOCK_NB) does.
func Glnx(path string) (*File, error) {
	for {
		fd, err := openRetry(path, unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NOCTTY, 0o600)
		if err != nil {
			return nil, fmt.Errorf("opening lock file %s: %w", path, err)
		}
		f := os.NewFile(uintptr(fd), path)
		if err := lockExclusive(fd); err != nil {
			f.Close()
			return nil, lockError(path, err)
		}
		// The previous exclusive owner may have removed the file before
		// closing it; a lock on it is worthless, so try again.
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			f.Close()
			return nil, err
		}
		if st.Nlink > 0 {
			return &File{f: f, path: path, unlink: true}, nil
		}
		f.Close()
	}
}

// lockExclusive is do_repo_lock (fd, LOCK_EX | LOCK_NB), which is also
// what glnx_make_lock_file() does: an OFD write lock on the whole file, or
// flock() if the kernel does not know OFD locks.
func lockExclusive(fd int) error {
	fl := unix.Flock_t{Type: unix.F_WRLCK, Whence: 0, Start: 0, Len: 0}
	err := retryEINTR(func() error { return unix.FcntlFlock(uintptr(fd), unix.F_OFD_SETLK, &fl) })
	if errors.Is(err, unix.EINVAL) {
		err = retryEINTR(func() error { return unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB) })
	}
	return err
}

func lockError(path string, err error) error {
	// F_OFD_SETLK fails with EAGAIN or EACCES on a conflict, flock() with
	// EWOULDBLOCK (EAGAIN on Linux).
	if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EACCES) {
		return ErrBusy
	}
	return fmt.Errorf("locking %s: %w", path, err)
}

func openRetry(path string, flags int, mode uint32) (int, error) {
	var fd int
	err := retryEINTR(func() error {
		var err error
		fd, err = unix.Open(path, flags, mode)
		return err
	})
	return fd, err
}

func retryEINTR(f func() error) error {
	for {
		if err := f(); !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

// Probe reports whether someone holds a lock on the file at path that an
// exclusive lock would conflict with, without taking any lock or creating
// the file: F_OFD_GETLK for a write lock over the whole file, on a
// read-only descriptor. A missing file is not locked. pid is the holder's
// for a classic POSIX lock and -1 for an OFD lock, whose holder the kernel
// does not name. flock() locks, which flatpak only falls back to on
// kernels without OFD locks, are not seen.
func Probe(path string) (busy bool, pid int, err error) {
	fd, err := openRetry(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, fmt.Errorf("opening lock file %s: %w", path, err)
	}
	// Closing a descriptor drops the process's classic POSIX locks on the
	// file; rpak never holds any, and OFD locks belong to their own
	// descriptions, so this releases nothing.
	defer unix.Close(fd)
	fl := unix.Flock_t{Type: unix.F_WRLCK, Whence: 0, Start: 0, Len: 0}
	if err := retryEINTR(func() error { return unix.FcntlFlock(uintptr(fd), unix.F_OFD_GETLK, &fl) }); err != nil {
		return false, 0, fmt.Errorf("probing lock file %s: %w", path, err)
	}
	if fl.Type == unix.F_UNLCK {
		return false, 0, nil
	}
	return true, int(fl.Pid), nil
}
