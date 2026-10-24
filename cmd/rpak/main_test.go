package main

// Integration tests: the test binary re-executes itself as rpak, with
// RPAK_FLATPAK pointing to a fake flatpak shell script and extra-data served
// by an httptest server.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/flatpark/rpak/internal/app"
)

func TestMain(m *testing.M) {
	if os.Getenv("RPAK_TEST_AS_MAIN") == "1" {
		os.Exit(app.Main("test"))
	}
	os.Exit(m.Run())
}

// fakeFlatpak answers queries from files in $FAKE_DIR. When run as
// update/install it records its argv (NUL-separated), checks that the files
// listed in $FAKE_DIR/expect are in place with the right checksum, and then
// behaves as $FAKE_DIR/mode says.
const fakeFlatpak = `#!/bin/sh
d="$FAKE_DIR"
echo "$*" >> "$d/calls"
inst=${2#--}
# The command is the first argument that is not a leading global option.
for a; do case "$a" in -v*|--verbose|--ostree-verbose) ;; *) cmd=$a; break ;; esac; done
case "$cmd" in
remotes)
	cat "$d/remotes-$inst" 2>/dev/null || { echo "error: no installation" >&2; exit 1; } ;;
remote-ls)
	cat "$d/updates-$inst" 2>/dev/null || { echo "error: no installation" >&2; exit 1; } ;;
remote-info)
	for a; do remote=$ref; ref=$a; done
	f="$d/info/$inst/$remote/$(echo "$ref" | tr / _)"
	[ -f "$f" ] || { echo "error: Nothing matches $ref in remote $remote" >&2; exit 1; }
	cat "$f" ;;
update|upgrade|install)
	printf '%s\0' "$@" > "$d/argv"
	: > "$d/check"
	if [ -f "$d/expect" ]; then
		while read -r path sum; do
			got=$(sha256sum "$path" 2>/dev/null | cut -d' ' -f1)
			[ "$got" = "$sum" ] && echo "ok $path" >> "$d/check" || echo "MISSING $path" >> "$d/check"
		done < "$d/expect"
	fi
	mode=$(cat "$d/mode" 2>/dev/null)
	case "$mode" in
	exit*) exit "${mode#exit }" ;;
	kill) kill -TERM $$ ;;
	trap)
		trap 'echo TERM > "$d/trapped"; exit 7' TERM
		touch "$d/started"
		i=0; while [ $i -lt 100 ]; do sleep 0.1; i=$((i+1)); done
		exit 99 ;;
	esac
	exit 0 ;;
uninstall|list|clean|--version)
	printf '%s\0' "$@" > "$d/argv"
	echo "fake flatpak $cmd"
	exit 5 ;;
*)
	echo "fake flatpak: unexpected $*" >&2; exit 64 ;;
esac
`

type env struct {
	t    *testing.T
	fake string // $FAKE_DIR
	base string // FLATPAK_USER_DIR
	srv  *httptest.Server
	hits atomic.Int64

	// Set up by the test before rpak runs; the race detector cannot see
	// that ordering across the process boundary, hence the lock.
	mu      sync.Mutex
	files   map[string][]byte
	handler func(w http.ResponseWriter, r *http.Request) bool
}

func (e *env) setHandler(h func(w http.ResponseWriter, r *http.Request) bool) {
	e.mu.Lock()
	e.handler = h
	e.mu.Unlock()
}

func (e *env) file(path string) ([]byte, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	b, ok := e.files[path]
	return b, ok
}

func newEnv(t *testing.T) *env {
	e := &env{t: t, fake: t.TempDir(), base: t.TempDir(), files: make(map[string][]byte)}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.hits.Add(1)
		e.mu.Lock()
		handler := e.handler
		e.mu.Unlock()
		if handler != nil && handler(w, r) {
			return
		}
		content, ok := e.file(r.URL.Path)
		if !ok {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(content))
	}))
	t.Cleanup(e.srv.Close)
	e.write("flatpak", fakeFlatpak)
	os.Chmod(filepath.Join(e.fake, "flatpak"), 0o755)
	return e
}

func (e *env) write(name, content string) {
	path := filepath.Join(e.fake, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) read(name string) string {
	b, _ := os.ReadFile(filepath.Join(e.fake, name))
	return string(b)
}

// app registers an extra-data app: remote-info output for inst/remote/ref
// (and for queryRef, the name used on the command line), and the file on
// the server. It returns the path flatpak will look at.
func (e *env) app(inst, remote, ref, queryRef string, size int) string {
	content := make([]byte, size)
	for i := range content {
		content[i] = byte(i*31) ^ byte(len(ref))
	}
	s := sha256.Sum256(content)
	checksum := hex.EncodeToString(s[:])
	id := strings.Split(ref, "/")[1]
	name := id + ".deb"
	e.mu.Lock()
	e.files["/dl/"+name] = content
	e.mu.Unlock()
	info := fmt.Sprintf("%s\n[Application]\nname=%s\nruntime=org.freedesktop.Platform/x86_64/24.08\n\n[Extra Data]\nname=%s\nchecksum=%s\nsize=%d\nuri=%s/dl/%s\n",
		ref, id, name, checksum, size, e.srv.URL, name)
	e.write(filepath.Join("info", inst, remote, strings.ReplaceAll(queryRef, "/", "_")), info)
	path := filepath.Join(e.base, "extra-data", checksum, name)
	f, _ := os.OpenFile(filepath.Join(e.fake, "expect"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	fmt.Fprintf(f, "%s %s\n", path, checksum)
	f.Close()
	return path
}

type outcome struct {
	code   int
	stderr string
}

func (e *env) command(argv ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], argv...)
	cmd.Env = append(os.Environ(),
		"RPAK_TEST_AS_MAIN=1",
		"RPAK_FLATPAK="+filepath.Join(e.fake, "flatpak"),
		"FAKE_DIR="+e.fake,
		"FLATPAK_USER_DIR="+e.base,
		"HOME="+e.t.TempDir(),
		"RPAK_DRY_RUN=",
		"RPAK_JOBS=",
	)
	return cmd
}

func (e *env) run(argv ...string) outcome {
	e.t.Helper()
	cmd := e.command(argv...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		e.t.Fatal(err)
	}
	return outcome{code, stderr.String()}
}

func (e *env) assertArgv(want []string) {
	e.t.Helper()
	got := e.read("argv")
	if got == "" {
		e.t.Fatal("flatpak update/install was not run")
	}
	if wantRaw := strings.Join(want, "\x00") + "\x00"; got != wantRaw {
		e.t.Fatalf("flatpak got argv %q, want %q", strings.Split(got, "\x00"), want)
	}
}

func extraDataDirs(t *testing.T, base string) []string {
	entries, err := os.ReadDir(filepath.Join(base, "extra-data"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestInstallPrefetchesAndPassesArgumentsVerbatim(t *testing.T) {
	e := newEnv(t)
	e.write("remotes-user", "flathub\nflatpark\n")
	e.app("user", "flatpark", "app/com.example.One/x86_64/stable", "com.example.One", 300_000)
	e.app("user", "flatpark", "app/com.example.Two/x86_64/stable", "com.example.Two//stable", 200_000)

	argv := []string{"install", "--user", "-y", "--subpath", "dir with spaces/ü", "--no-related", "flatpark", "com.example.One", "com.example.Two//stable"}
	out := e.run(argv...)
	if out.code != 0 {
		t.Fatalf("exit %d\n%s", out.code, out.stderr)
	}
	e.assertArgv(argv)
	if check := e.read("check"); strings.Contains(check, "MISSING") || strings.Count(check, "ok ") != 2 {
		t.Fatalf("files were not in place when flatpak ran:\n%s\nstderr:\n%s", check, out.stderr)
	}
	for _, want := range []string{"prefetching 2 extra-data file(s)", "start com.example.One.deb", "done com.example.Two.deb", "2 ready (2 downloaded, 0 reused), 0 skipped/failed"} {
		if !strings.Contains(out.stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, out.stderr)
		}
	}
	// flatpak succeeded: everything rpak placed is gone again.
	if dirs := extraDataDirs(t, e.base); len(dirs) != 0 {
		t.Errorf("left behind: %v", dirs)
	}
}

func TestUpdate(t *testing.T) {
	e := newEnv(t)
	e.write("updates-user", "app/com.example.One/x86_64/stable\tflatpark\napp/com.example.Two/x86_64/stable\tflatpark\n")
	e.write("updates-system", "app/com.example.Three/x86_64/stable\tflatpark\n")
	e.app("user", "flatpark", "app/com.example.One/x86_64/stable", "app/com.example.One/x86_64/stable", 10_000)
	e.app("system", "flatpark", "app/com.example.Three/x86_64/stable", "app/com.example.Three/x86_64/stable", 10_000)
	os.Remove(filepath.Join(e.fake, "expect"))

	// Only the app named on the command line, and only that file is fetched.
	out := e.run("update", "com.example.One")
	if out.code != 0 {
		t.Fatalf("exit %d\n%s", out.code, out.stderr)
	}
	e.assertArgv([]string{"update", "com.example.One"})
	if !strings.Contains(out.stderr, "prefetching 1 extra-data file(s)") || e.hits.Load() != 1 {
		t.Errorf("hits %d, stderr:\n%s", e.hits.Load(), out.stderr)
	}

	// No filter: user and system installations are both covered.
	e.hits.Store(0)
	out = e.run("update", "-y")
	if out.code != 0 || e.hits.Load() != 2 || !strings.Contains(out.stderr, "prefetching 2 extra-data file(s)") {
		t.Errorf("exit %d, hits %d, stderr:\n%s", out.code, e.hits.Load(), out.stderr)
	}
}

func TestFlatpakFailureKeepsFiles(t *testing.T) {
	e := newEnv(t)
	e.write("remotes-user", "flatpark\n")
	path := e.app("user", "flatpark", "app/com.example.One/x86_64/stable", "com.example.One", 50_000)
	e.write("mode", "exit 1")

	out := e.run("install", "--user", "com.example.One")
	if out.code != 1 {
		t.Fatalf("exit %d, want 1\n%s", out.code, out.stderr)
	}
	if !strings.Contains(e.read("check"), "ok ") {
		t.Fatalf("file not in place: %s", e.read("check"))
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("prefetched file removed after a failed flatpak run: %v", err)
	}

	// The next run reuses it without downloading.
	e.hits.Store(0)
	e.write("mode", "exit 0")
	out = e.run("install", "--user", "com.example.One")
	if out.code != 0 || e.hits.Load() != 0 || !strings.Contains(out.stderr, "1 reused") {
		t.Fatalf("exit %d, hits %d\n%s", out.code, e.hits.Load(), out.stderr)
	}
	if dirs := extraDataDirs(t, e.base); len(dirs) != 0 {
		t.Errorf("left behind: %v", dirs)
	}
}

func TestFlatpakKilledBySignal(t *testing.T) {
	e := newEnv(t)
	e.write("mode", "kill")
	out := e.run("update", "--user")
	if out.code != 128+int(syscall.SIGTERM) {
		t.Fatalf("exit %d, want %d\n%s", out.code, 128+int(syscall.SIGTERM), out.stderr)
	}
}

func TestServerErrorStillRunsFlatpak(t *testing.T) {
	e := newEnv(t)
	e.write("remotes-user", "flatpark\n")
	path := e.app("user", "flatpark", "app/com.example.One/x86_64/stable", "com.example.One", 50_000)
	e.setHandler(func(w http.ResponseWriter, r *http.Request) bool {
		http.Error(w, "broken", http.StatusInternalServerError)
		return true
	})
	argv := []string{"install", "--user", "-y", "flatpark", "com.example.One"}
	start := time.Now()
	out := e.run(argv...)
	if out.code != 0 {
		t.Fatalf("exit %d\n%s", out.code, out.stderr)
	}
	e.assertArgv(argv)
	if !strings.Contains(out.stderr, "500") || !strings.Contains(out.stderr, "flatpak will download it") {
		t.Errorf("stderr:\n%s", out.stderr)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("file exists after failed download: %v", err)
	}
	if e.hits.Load() != 3 {
		t.Errorf("requests = %d, want 3 (2 retries)", e.hits.Load())
	}
	t.Logf("took %v with retries", time.Since(start))
}

func TestQueryFailuresStillRunFlatpak(t *testing.T) {
	// No remotes/updates files: every query fails.
	e := newEnv(t)
	for _, argv := range [][]string{
		{"update"},
		{"install", "flathub", "org.example.App"},
		{"install", "--installation=nope", "org.example.App"},
	} {
		out := e.run(argv...)
		if out.code != 0 {
			t.Fatalf("%q: exit %d\n%s", argv, out.code, out.stderr)
		}
		e.assertArgv(argv)
	}
}

func TestNothingToPrefetchIsSilent(t *testing.T) {
	e := newEnv(t)
	e.write("updates-user", "app/org.example.Plain/x86_64/stable\tflathub\n")
	e.write("info/user/flathub/app_org.example.Plain_x86_64_stable", "app/org.example.Plain/x86_64/stable\n[Application]\nname=org.example.Plain\n")
	out := e.run("update", "--user")
	if out.code != 0 || out.stderr != "" {
		t.Fatalf("exit %d, stderr %q", out.code, out.stderr)
	}
	e.assertArgv([]string{"update", "--user"})
}

func TestSkipConditionsRunNoQueries(t *testing.T) {
	e := newEnv(t)
	for _, argv := range [][]string{
		{"install", "--from", "https://example.com/app.flatpakref"},
		{"install", "./app.flatpak"},
		{"update", "--commit=0123", "org.example.App"},
		{"update", "--no-pull"},
	} {
		os.Remove(filepath.Join(e.fake, "calls"))
		out := e.run(argv...)
		if out.code != 0 {
			t.Fatalf("%q: exit %d\n%s", argv, out.code, out.stderr)
		}
		e.assertArgv(argv)
		if calls := strings.TrimSpace(e.read("calls")); calls != strings.Join(argv, " ") {
			t.Errorf("%q: flatpak calls:\n%s", argv, calls)
		}
	}
}

func TestSignalIsForwardedToFlatpak(t *testing.T) {
	e := newEnv(t)
	e.write("mode", "trap")
	cmd := e.command("update", "--user")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, filepath.Join(e.fake, "started"))
	cmd.Process.Signal(syscall.SIGTERM)
	err := cmd.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("rpak: %v, want exit status 7", err)
	}
	if got := strings.TrimSpace(e.read("trapped")); got != "TERM" {
		t.Fatalf("flatpak did not get SIGTERM: %q", got)
	}
}

func TestInterruptDuringPrefetch(t *testing.T) {
	e := newEnv(t)
	e.write("remotes-user", "flatpark\n")
	path := e.app("user", "flatpark", "app/com.example.One/x86_64/stable", "com.example.One", 100_000)
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	e.setHandler(func(w http.ResponseWriter, r *http.Request) bool {
		content, _ := e.file(r.URL.Path)
		w.Header().Set("Content-Length", fmt.Sprint(len(content)))
		w.Write(content[:40_000])
		w.(http.Flusher).Flush()
		close(started)
		<-release
		return true
	})
	cmd := e.command("install", "--user", "flatpark", "com.example.One")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("download did not start")
	}
	time.Sleep(200 * time.Millisecond)
	cmd.Process.Signal(syscall.SIGINT)
	err := cmd.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 130 {
		t.Fatalf("rpak: %v, want exit status 130\n%s", err, stderr.String())
	}
	if e.read("argv") != "" {
		t.Fatal("flatpak install ran after SIGINT")
	}
	if info, err := os.Stat(path + ".part"); err != nil || info.Size() != 40_000 {
		t.Fatalf(".part not kept: %v %v", info, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("incomplete file under the real name: %v", err)
	}
}

func TestDryRun(t *testing.T) {
	e := newEnv(t)
	e.write("remotes-user", "flatpark\n")
	e.app("user", "flatpark", "app/com.example.One/x86_64/stable", "com.example.One", 1000)
	cmd := e.command("install", "--user", "flatpark", "com.example.One")
	cmd.Env = append(cmd.Env, "RPAK_DRY_RUN=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if e.read("argv") != "" || e.hits.Load() != 0 || len(extraDataDirs(t, e.base)) != 0 {
		t.Fatalf("dry run did something: argv %q, hits %d", e.read("argv"), e.hits.Load())
	}
	for _, want := range []string{"com.example.One.deb", "would run: " + filepath.Join(e.fake, "flatpak") + " install --user flatpark com.example.One"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestUsage(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		argv []string
		code int
	}{
		{nil, 2}, {[]string{"help"}, 0}, {[]string{"-h"}, 0}, {[]string{"--help"}, 0},
	} {
		cmd := e.command(c.argv...)
		out, _ := cmd.CombinedOutput()
		if cmd.ProcessState.ExitCode() != c.code {
			t.Errorf("%q: exit %d, want %d", c.argv, cmd.ProcessState.ExitCode(), c.code)
		}
		if !strings.Contains(string(out), "update") || !strings.Contains(string(out), "install") {
			t.Errorf("%q: no usage:\n%s", c.argv, out)
		}
	}
	if e.read("calls") != "" {
		t.Error("flatpak was run")
	}
}

func TestOtherCommandsPassThrough(t *testing.T) {
	e := newEnv(t)
	for _, argv := range [][]string{
		{"uninstall", "--user", "-y", "com.example.One"},
		{"list", "--columns=application"},
		{"clean", "--dry-run"},
		{"-v", "list"}, // a global option first, then a command rpak does not handle
	} {
		os.Remove(filepath.Join(e.fake, "calls"))
		out := e.run(argv...)
		if out.code != 5 {
			t.Fatalf("%q: exit %d, want flatpak's 5\n%s", argv, out.code, out.stderr)
		}
		e.assertArgv(argv)
		// No planning queries: flatpak is the only thing that ran.
		if calls := strings.TrimSpace(e.read("calls")); calls != strings.Join(argv, " ") {
			t.Errorf("%q: calls %q", argv, calls)
		}
	}

	cmd := e.command("--version")
	out, _ := cmd.CombinedOutput()
	if cmd.ProcessState.ExitCode() != 5 || string(out) != "rpak test\nfake flatpak --version\n" {
		t.Errorf("--version: exit %d, output %q", cmd.ProcessState.ExitCode(), out)
	}
}

func TestLeadingGlobalOptionsAndUpgradeAlias(t *testing.T) {
	e := newEnv(t)
	e.write("remotes-user", "flatpark\n")
	e.app("user", "flatpark", "app/com.example.One/x86_64/stable", "com.example.One", 10_000)
	argv := []string{"-vv", "--ostree-verbose", "install", "--user", "-y", "flatpark", "com.example.One"}
	out := e.run(argv...)
	if out.code != 0 || !strings.Contains(out.stderr, "prefetching 1 extra-data file(s)") {
		t.Fatalf("exit %d\n%s", out.code, out.stderr)
	}
	e.assertArgv(argv)
	if check := e.read("check"); !strings.Contains(check, "ok ") {
		t.Fatalf("file not in place: %s", check)
	}

	os.Remove(filepath.Join(e.fake, "expect"))
	e.write("updates-user", "app/com.example.One/x86_64/stable\tflatpark\n")
	e.app("user", "flatpark", "app/com.example.One/x86_64/stable", "app/com.example.One/x86_64/stable", 10_000)
	out = e.run("upgrade", "--user", "-y")
	if out.code != 0 || !strings.Contains(out.stderr, "prefetching 1 extra-data file(s)") {
		t.Fatalf("upgrade: exit %d\n%s", out.code, out.stderr)
	}
	e.assertArgv([]string{"upgrade", "--user", "-y"})
}

func TestHelpDoesNotPrefetch(t *testing.T) {
	e := newEnv(t)
	e.write("updates-user", "app/com.example.One/x86_64/stable\tflatpark\n")
	e.app("user", "flatpark", "app/com.example.One/x86_64/stable", "app/com.example.One/x86_64/stable", 10_000)
	for _, argv := range [][]string{{"update", "--user", "--help"}, {"update", "--user", "-h"}} {
		e.hits.Store(0)
		out := e.run(argv...)
		if out.code != 0 || e.hits.Load() != 0 || strings.Contains(out.stderr, "prefetching") {
			t.Errorf("%q: exit %d, hits %d\n%s", argv, out.code, e.hits.Load(), out.stderr)
		}
		e.assertArgv(argv)
	}
}

func waitFor(t *testing.T, path string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s did not appear", path)
}
