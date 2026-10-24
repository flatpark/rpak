package prefetch

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// MarkerName marks an extra-data/<sha256>/ directory as created by rpak.
// Only directories carrying it are ever written to or removed; anything
// else there belongs to the user or another tool.
const MarkerName = ".rpak-prefetch"

// MaxAge is how long a marked directory left behind by an earlier run
// (flatpak failed, the user declined, rpak was killed) is kept for reuse.
const MaxAge = 72 * time.Hour

// tmpPrefix names directories being set up; see claimDir.
const tmpPrefix = ".rpak-tmp-"

func hasMarker(dir string) bool {
	info, err := os.Lstat(filepath.Join(dir, MarkerName))
	return err == nil && info.Mode().IsRegular()
}

// RemoveDirs removes the given extra-data/<sha256>/ directories of baseDir
// that carry the rpak marker; others are left alone.
func RemoveDirs(baseDir string, dirs []string) error {
	root := filepath.Join(baseDir, "extra-data")
	var errs []error
	for _, dir := range dirs {
		dir = filepath.Clean(dir)
		if filepath.Dir(dir) != root || !hasMarker(dir) {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// RemoveExpired removes marked directories whose marker is older than
// maxAge. The marker's mtime is refreshed whenever a run uses the directory.
func RemoveExpired(baseDir string, now time.Time, maxAge time.Duration) error {
	root := filepath.Join(baseDir, "extra-data")
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		info, err := os.Lstat(filepath.Join(dir, MarkerName))
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if now.Sub(info.ModTime()) > maxAge {
			if err := os.RemoveAll(dir); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// claimDir makes sure dir exists and carries the rpak marker. It reports
// false, without changing anything, if dir already exists without one.
//
// A new directory is prepared under a temporary name with the marker inside
// and then renamed into place, so an unmarked <sha256>/ directory of ours
// cannot be left behind; a stray temporary one is marked and expires.
func claimDir(dir, markerContents string) (bool, error) {
	info, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return false, err
		}
		tmp, err := os.MkdirTemp(filepath.Dir(dir), tmpPrefix)
		if err != nil {
			return false, err
		}
		if err := os.Chmod(tmp, 0o755); err != nil {
			os.RemoveAll(tmp)
			return false, err
		}
		if err := os.WriteFile(filepath.Join(tmp, MarkerName), []byte(markerContents), 0o644); err != nil {
			os.RemoveAll(tmp)
			return false, err
		}
		if err := os.Rename(tmp, dir); err != nil {
			// Lost a race with someone creating the same directory.
			os.RemoveAll(tmp)
			info, err = os.Lstat(dir)
			if err != nil {
				return false, err
			}
		} else {
			return true, syncDir(filepath.Dir(dir))
		}
	} else if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, nil
	}
	if !hasMarker(dir) {
		return false, nil
	}
	// Restart the expiry clock: this directory is in use again.
	now := time.Now()
	if err := os.Chtimes(filepath.Join(dir, MarkerName), now, now); err != nil {
		return false, fmt.Errorf("refresh marker: %w", err)
	}
	return true, nil
}
