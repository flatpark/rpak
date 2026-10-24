package prefetch

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// ErrInUse means that another rpak is using the prefetch directories.
var ErrInUse = errors.New("another rpak is using the prefetched files")

// Locking is between rpak processes only, with flock() on the
// <user base dir>/extra-data directory itself: a run holds it shared from
// before it looks at the directories until it has removed them after
// flatpak, and `rpak clean` takes it exclusively, without waiting, before
// deleting marked directories, so it never deletes a download in progress
// or a file flatpak is about to read.

// LockShared takes the shared lock, waiting for a running `rpak clean`.
// With create, the extra-data directory is created if needed; without, a
// missing directory is not locked (locked is false) and release does
// nothing.
func LockShared(baseDir string, create bool) (release func(), locked bool, err error) {
	return lockDir(baseDir, create, unix.LOCK_SH)
}

// TryLockExclusive takes the exclusive lock, or fails with ErrInUse.
// A missing directory is not locked.
func TryLockExclusive(baseDir string) (release func(), err error) {
	release, _, err = lockDir(baseDir, false, unix.LOCK_EX|unix.LOCK_NB)
	return release, err
}

func lockDir(baseDir string, create bool, how int) (func(), bool, error) {
	dir := filepath.Join(baseDir, "extra-data")
	if create {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, false, err
		}
	}
	f, err := os.Open(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return func() {}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	for {
		err = unix.Flock(int(f.Fd()), how)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, false, ErrInUse
		}
		return nil, false, err
	}
	return func() { f.Close() }, true, nil
}
