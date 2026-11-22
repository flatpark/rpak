package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Files replaced upstream under the same URL (internal/app/changed.go).

const oneRef = "app/com.example.One/x86_64/stable"

// replacedApp sets up com.example.One in the user installation's remote
// flatpark, and serves another file at its URL: with another size when
// longer, else of the same size. It returns the file served.
func replacedApp(e *env, longer bool) []byte {
	e.write("remotes-user", "flatpark\n")
	os.MkdirAll(filepath.Join(e.base, "repo"), 0o755)
	os.WriteFile(filepath.Join(e.base, "repo", "config"),
		[]byte("[core]\nrepo_version=1\n\n[remote \"flatpark\"]\nurl=file:///nonexistent\n"), 0o644)
	e.app("user", "flatpark", oneRef, "com.example.One", 50_000)
	os.Remove(filepath.Join(e.fake, "expect"))
	// What the patch flow asks (remote-info --show-commit of the full ref).
	info := e.read(filepath.Join("info", "user", "flatpark", "com.example.One"))
	_, rest, _ := strings.Cut(info, "\n")
	e.write(filepath.Join("info", "user", "flatpark", strings.ReplaceAll(oneRef, "/", "_")), oneRef+" "+strings.Repeat("a", 64)+"\n"+rest)
	n := 50_000
	if longer {
		n = 60_000
	}
	served := make([]byte, n)
	for i := range served {
		served[i] = byte(i * 13)
	}
	e.mu.Lock()
	e.files["/dl/com.example.One.deb"] = served
	e.mu.Unlock()
	return served
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// The server announces another size: rpak says so before downloading
// anything, and without a terminal says what to run; flatpak runs as
// asked.
func TestReplacedFileFoundByProbe(t *testing.T) {
	e := newEnv(t)
	replacedApp(e, true)
	argv := []string{"install", "--user", "-y", "flatpark", "com.example.One"}
	out := e.run(argv...)
	if out.code != 0 {
		t.Fatalf("exit %d\n%s", out.code, out.stderr)
	}
	e.assertArgv(argv)
	url := e.srv.URL + "/dl/com.example.One.deb"
	for _, want := range []string{
		"rpak: com.example.One: com.example.One.deb is not the file the app was built with; upstream has likely replaced it at the same URL.\n",
		"rpak:   URL:        " + url + "\n",
		"rpak:   served now: 60.0 kB, sha256 not known until downloaded\n",
		"rpak: To install it with the file as it is now (experimental): rpak install --user --extra-data=com.example.One.deb=" + url + " flatpark com.example.One\n",
	} {
		if !strings.Contains(out.stderr, want) {
			t.Errorf("no %q in:\n%s", want, out.stderr)
		}
	}
	if e.hits.Load() != 0 || e.heads.Load() != 1 {
		t.Errorf("%d downloads, %d probes", e.hits.Load(), e.heads.Load())
	}
}

// Same size, other content: found once downloaded, and kept under its own
// checksum for the patch.
func TestReplacedFileFoundByDownload(t *testing.T) {
	e := newEnv(t)
	served := replacedApp(e, false)
	out := e.run("install", "--user", "-y", "flatpark", "com.example.One")
	if out.code != 0 {
		t.Fatalf("exit %d\n%s", out.code, out.stderr)
	}
	for _, want := range []string{
		"warning: com.example.One.deb was replaced upstream: the server has 50.0 kB, sha256 " + sha(served),
		"1 replaced upstream",
		"rpak:   served now: 50.0 kB, sha256 " + sha(served) + "\n",
		"rpak: To install it with the file as it is now (experimental): rpak install --user --extra-data=",
	} {
		if !strings.Contains(out.stderr, want) {
			t.Errorf("no %q in:\n%s", want, out.stderr)
		}
	}
	if e.hits.Load() != 1 {
		t.Errorf("%d downloads, want 1: a replaced file is not retried", e.hits.Load())
	}
	kept := filepath.Join(e.base, "extra-data", sha(served), "com.example.One.deb")
	if b, err := os.ReadFile(kept); err != nil || sha(b) != sha(served) {
		t.Errorf("not kept at %s: %v", kept, err)
	}
}

// On a terminal rpak asks; "n" leaves the command as it was.
func TestReplacedFileDeclined(t *testing.T) {
	e := newEnv(t)
	replacedApp(e, true)
	e.write("mode", "exit 1")
	tm := newTerminal(t)
	r := e.startOn(tm, nil, "install", "--user", "flatpark", "com.example.One")
	question := "Install com.example.One with the file as it is now, as an experimental extra-data patch (its packagers did not check it)? [y/N]: "
	eventually(t, "the question", func() bool { return strings.Contains(tm.output(), question) }, tm.output)
	if e.read("argv") != "" {
		t.Fatal("flatpak runs while rpak asks")
	}
	tm.typeIn("n\r")
	if code := r.wait(tm); code != 1 {
		t.Fatalf("exit %d, want flatpak's\n%s", code, tm.output())
	}
	if !strings.Contains(tm.output(), "rpak: Not patched; flatpak will fail on it. Later: rpak install --user") {
		t.Errorf("output:\n%s", tm.output())
	}
	if e.read("argv") == "" || e.hits.Load() != 0 {
		t.Errorf("flatpak did not run, or rpak downloaded the declined file (%d)", e.hits.Load())
	}
}

// "y" patches the app from the same URL instead of running the command,
// with the file rpak downloaded already.
func TestReplacedFileAccepted(t *testing.T) {
	e := newEnv(t)
	replacedApp(e, false)
	tm := newTerminal(t)
	// Sequential (-y would not ask): stdin is the terminal, stdout is not.
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()
	cmd := e.command("install", "--user", "flatpark", "com.example.One")
	cmd.Env = append(cmd.Env, "TERM=xterm", "NO_COLOR=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tm.slave, devnull, tm.slave
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	eventually(t, "the question", func() bool { return strings.Contains(tm.output(), "[y/N]: ") }, tm.output)
	tm.typeIn("y\r")
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("rpak did not exit:\n%s", tm.output())
	}
	tm.settle()
	if e.hits.Load() != 1 {
		t.Errorf("%d downloads, want 1: the patch uses the file downloaded\n%s", e.hits.Load(), tm.output())
	}
	// The patch flow got as far as the reexport (the fake remote has no
	// objects; test/e2e has the whole flow).
	if !strings.Contains(tm.output(), "rpak: reexporting com.example.One aaaaaaaaaaaa from flatpark with runtime org.freedesktop.Platform/x86_64/24.08, extra-data com.example.One.deb") {
		t.Errorf("no patch flow:\n%s", tm.output())
	}
	if got := e.read("argv"); strings.Contains(got, "flatpark\x00com.example.One") {
		t.Errorf("the command ran as given: %q", got)
	}
}

// Interactive mode, found once downloaded while flatpak asks: the yes is
// not passed on (flatpak would download the file and fail), rpak asks its
// question, and after a no runs the command again, which leaves that file
// to flatpak.
func TestReplacedFileStopsFlatpak(t *testing.T) {
	e := newEnv(t)
	replacedApp(e, false)
	e.write("mode", "prompt")
	tm := newTerminal(t)
	r := e.startOn(tm, nil, "install", "--user", "flatpark", "com.example.One")
	eventually(t, "flatpak's question", func() bool { return strings.Contains(tm.output(), prompt) }, tm.output)
	eventually(t, "the download", func() bool { return e.hits.Load() == 1 }, tm.output)
	time.Sleep(200 * time.Millisecond)
	tm.typeIn("y\r")
	eventually(t, "rpak's question", func() bool { return strings.Contains(tm.output(), "[y/N]: ") }, tm.output)
	if got := e.read("answer"); got != "<EOF>\n" {
		t.Errorf("flatpak got %q, want the end of its input", got)
	}
	if !strings.Contains(tm.output(), "rpak: Stopped flatpak at its question: it would download a file that was replaced upstream, and fail.") {
		t.Errorf("output:\n%s", tm.output())
	}
	os.Remove(filepath.Join(e.fake, "answer"))
	tm.typeIn("n\r")
	eventually(t, "flatpak's question again", func() bool { return strings.Count(tm.output(), prompt) == 2 }, tm.output)
	tm.typeIn("y\r")
	if code := r.wait(tm); code != 0 {
		t.Fatalf("exit %d\n%s", code, tm.output())
	}
	out := tm.output()
	if !strings.Contains(out, "rpak: Running the command again") || e.read("answer") != "y\n" {
		t.Errorf("answer %q, output:\n%s", e.read("answer"), out)
	}
	if e.hits.Load() != 1 || strings.Count(out, "[y/N]: ") != 1 {
		t.Errorf("%d downloads; asked again?\n%s", e.hits.Load(), out)
	}
}
