package prefetch

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUserBaseDirPriority(t *testing.T) {
	home := func() (string, error) { return "/home/u", nil }
	cases := []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"FLATPAK_USER_DIR": "/fp", "XDG_DATA_HOME": "/xdg"}, "/fp"},
		{map[string]string{"FLATPAK_USER_DIR": "", "XDG_DATA_HOME": "/xdg"}, "/xdg/flatpak"},
		{map[string]string{"XDG_DATA_HOME": ""}, "/home/u/.local/share/flatpak"},
		{nil, "/home/u/.local/share/flatpak"},
	}
	for _, c := range cases {
		got, err := UserBaseDir(func(k string) string { return c.env[k] }, home)
		if err != nil || got != c.want {
			t.Errorf("env %v: got %q, %v; want %q", c.env, got, err, c.want)
		}
	}
}

// layout creates extra-data/<name>/file directories, marked or not, with
// the marker's mtime set to age ago.
func layout(t *testing.T, base string, dirs map[string]bool, age map[string]time.Duration) map[string]string {
	paths := make(map[string]string)
	for name, marked := range dirs {
		dir := filepath.Join(base, "extra-data", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, "file"), []byte("x"), 0o644)
		if marked {
			marker := filepath.Join(dir, MarkerName)
			os.WriteFile(marker, []byte("rpak test\n"), 0o644)
			when := time.Now().Add(-age[name])
			os.Chtimes(marker, when, when)
		}
		paths[name] = dir
	}
	return paths
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func TestRemoveDirs(t *testing.T) {
	base := t.TempDir()
	p := layout(t, base, map[string]bool{"ours": true, "theirs": false, "untouched": true}, nil)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, MarkerName), nil, 0o644)

	err := RemoveDirs(base, []string{p["ours"], p["theirs"], outside, filepath.Join(base, "extra-data")})
	if err != nil {
		t.Fatal(err)
	}
	if exists(p["ours"]) {
		t.Error("marked directory was not removed")
	}
	if !exists(filepath.Join(p["theirs"], "file")) {
		t.Error("unmarked directory was removed")
	}
	if !exists(p["untouched"]) {
		t.Error("marked directory not in the list was removed")
	}
	if !exists(outside) {
		t.Error("directory outside extra-data/ was removed")
	}
}

func TestRemoveExpired(t *testing.T) {
	base := t.TempDir()
	p := layout(t, base,
		map[string]bool{"old": true, "fresh": true, "old-unmarked": false},
		map[string]time.Duration{"old": MaxAge + time.Hour, "fresh": MaxAge - time.Hour})
	ancient := time.Now().Add(-30 * 24 * time.Hour)
	os.Chtimes(p["old-unmarked"], ancient, ancient)
	os.Chtimes(filepath.Join(p["old-unmarked"], "file"), ancient, ancient)

	if err := RemoveExpired(base, time.Now(), MaxAge); err != nil {
		t.Fatal(err)
	}
	if exists(p["old"]) {
		t.Error("expired directory was not removed")
	}
	if !exists(p["fresh"]) {
		t.Error("fresh directory was removed")
	}
	if !exists(filepath.Join(p["old-unmarked"], "file")) {
		t.Error("unmarked directory was removed")
	}
	// No extra-data directory at all is fine.
	if err := RemoveExpired(t.TempDir(), time.Now(), MaxAge); err != nil {
		t.Error(err)
	}
}

func TestClaimDir(t *testing.T) {
	base := t.TempDir()
	dir := ExtraDataDir(base, "abc")
	if ok, err := claimDir(dir, "m\n"); !ok || err != nil {
		t.Fatalf("new dir: %v %v", ok, err)
	}
	if !hasMarker(dir) {
		t.Fatal("no marker")
	}
	if ok, err := claimDir(dir, "m\n"); !ok || err != nil {
		t.Fatalf("marked dir: %v %v", ok, err)
	}
	foreign := ExtraDataDir(base, "def")
	os.MkdirAll(foreign, 0o755)
	if ok, err := claimDir(foreign, "m\n"); ok || err != nil {
		t.Fatalf("unmarked dir: %v %v", ok, err)
	}
	if exists(filepath.Join(foreign, MarkerName)) {
		t.Fatal("unmarked dir was claimed")
	}
	entries, _ := os.ReadDir(filepath.Join(base, "extra-data"))
	if len(entries) != 2 {
		t.Errorf("leftovers in extra-data: %v", entries)
	}
}
