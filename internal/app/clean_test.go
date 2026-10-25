package app

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCleanDeleteDataUnderSudo: "sudo rpak clean --delete-data" lists the
// data of apps but deletes nothing and resets no permissions, whichever
// home directory sudo left in $HOME; --user is refused as before.
func TestCleanDeleteDataUnderSudo(t *testing.T) {
	home, user, fake := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("FLATPAK_USER_DIR", user)
	t.Setenv("FLATPAK_SYSTEM_DIR", t.TempDir())
	t.Setenv("FLATPAK_CONFIG_DIR", t.TempDir())
	calls := filepath.Join(fake, "calls")
	flatpak := filepath.Join(fake, "flatpak")
	if err := os.WriteFile(flatpak, []byte("#!/bin/sh\necho \"$*\" >> "+calls+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RPAK_FLATPAK", flatpak)
	t.Setenv("SUDO_COMMAND", "/usr/local/bin/rpak clean --delete-data")
	geteuid = func() int { return 0 }
	t.Cleanup(func() { geteuid = os.Geteuid })

	data := filepath.Join(home, ".var", "app", "org.example.Gone", "file")
	override := filepath.Join(user, "overrides", "org.example.Gone")
	for _, p := range []string{data, override} {
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for _, argv := range [][]string{{"--delete-data"}, {"--delete-data", "--dry-run"}} {
		var stdout, stderr bytes.Buffer
		if code := cleanCommand(argv, &stdout, &stderr); code != 0 {
			t.Fatalf("%q: exit %d\n%s%s", argv, code, stdout.String(), stderr.String())
		}
		if !strings.Contains(stderr.String(), `not deleting the data of apps under sudo; run "rpak clean --delete-data" without sudo`) {
			t.Errorf("%q: stderr:\n%s", argv, stderr.String())
		}
		if !strings.Contains(stdout.String(), "org.example.Gone  ~/.var/app, overrides\n  Kept ") ||
			!strings.Contains(stdout.String(), "\n\nNothing ") {
			t.Errorf("%q: output:\n%s", argv, stdout.String())
		}
	}
	for _, p := range []string{data, override} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s deleted under sudo", p)
		}
	}
	if b, _ := os.ReadFile(calls); len(b) != 0 {
		t.Errorf("flatpak was run: %s", b)
	}

	var stdout, stderr bytes.Buffer
	if code := cleanCommand([]string{"--user", "--delete-data"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "refusing to operate under sudo with --user") {
		t.Errorf("--user: exit %d\n%s%s", code, stdout.String(), stderr.String())
	}

	// Not run by sudo as rpak (a root shell from "sudo -i"): root's own
	// data is root's to delete.
	t.Setenv("SUDO_COMMAND", "/bin/bash")
	stdout.Reset()
	stderr.Reset()
	if code := cleanCommand([]string{"--delete-data"}, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Errorf("root shell: exit %d\n%s%s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Lstat(data); err == nil {
		t.Error("not deleted in a root shell")
	}
}
