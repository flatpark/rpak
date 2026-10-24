// Package prefetch downloads extra-data files to where flatpak looks for
// them before downloading them itself, and removes them again.
//
// flatpak_dir_pull_extra_data_to_bytes() (common/flatpak-dir.c) loads
// <user base dir>/extra-data/<sha256>/<name> if that file exists. It reads the
// whole file and fails the pull, without falling back to downloading, if the
// size or the checksum is wrong. So a file under that name must only ever
// appear complete and verified: downloads go to <name>.part and are renamed
// once checked. A file that is merely missing or misnamed is harmless,
// flatpak downloads it as usual.
package prefetch

import (
	"os"
	"path/filepath"
)

// UserBaseDir mirrors flatpak_get_user_base_dir_location(): $FLATPAK_USER_DIR
// if set and not empty, else g_get_user_data_dir()/flatpak, which is
// $XDG_DATA_HOME/flatpak or ~/.local/share/flatpak.
//
// flatpak uses the user base dir of whoever runs it even when installing
// into the system installation, so this is the right place either way.
func UserBaseDir(getenv func(string) string, homeDir func() (string, error)) (string, error) {
	if dir := getenv("FLATPAK_USER_DIR"); dir != "" {
		return dir, nil
	}
	if dir := getenv("XDG_DATA_HOME"); dir != "" {
		return filepath.Join(dir, "flatpak"), nil
	}
	home, err := homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "flatpak"), nil
}

// DefaultUserBaseDir is UserBaseDir for the current process. Like
// g_get_home_dir(), it prefers $HOME and falls back to the passwd entry.
func DefaultUserBaseDir() (string, error) {
	return UserBaseDir(os.Getenv, os.UserHomeDir)
}

// ExtraDataDir is the directory flatpak looks in for a file with this
// checksum.
func ExtraDataDir(baseDir, checksum string) string {
	return filepath.Join(baseDir, "extra-data", checksum)
}
