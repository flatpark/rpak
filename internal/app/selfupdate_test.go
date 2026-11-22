package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/flatpark/rpak/internal/release"
)

// releaseServer serves releases as GitHub does (see release.Client), with
// the binary of each tag, and records the requests.
type releaseServer struct {
	*httptest.Server
	mu     sync.Mutex
	latest string
	sums   map[string]string // SHA256SUMS by tag, if not the right one
	reqs   []string
}

func newReleaseServer(t *testing.T, latest string, tags ...string) *releaseServer {
	s := &releaseServer{latest: latest, sums: map[string]string{}}
	known := map[string]bool{}
	for _, tag := range tags {
		known[tag] = true
	}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.reqs = append(s.reqs, r.URL.Path)
		latest, sums := s.latest, s.sums
		s.mu.Unlock()
		if r.URL.Path == "/releases/latest" {
			http.Redirect(w, r, "/releases/tag/"+latest, http.StatusFound)
			return
		}
		rest, ok := strings.CutPrefix(r.URL.Path, "/releases/download/")
		tag, name, _ := strings.Cut(rest, "/")
		if !ok || !known[tag] {
			http.NotFound(w, r)
			return
		}
		switch name {
		case "SHA256SUMS":
			if s, ok := sums[tag]; ok {
				fmt.Fprint(w, s)
				return
			}
			for _, arch := range []string{"amd64", "arm64"} {
				fmt.Fprintf(w, "%s  rpak-linux-%s\n", sha(binary(tag, arch)), arch)
			}
		case "rpak-linux-amd64", "rpak-linux-arm64":
			fmt.Fprint(w, binary(tag, strings.TrimPrefix(name, "rpak-linux-")))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// binary is the content of the binary of a release.
func binary(tag, arch string) string { return "#!/bin/sh\necho rpak " + tag + " " + arch + "\n" }

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func (s *releaseServer) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.reqs...)
}

// downloads are the requests for a release's files.
func (s *releaseServer) downloads() []string {
	var out []string
	for _, r := range s.requests() {
		if strings.HasPrefix(r, "/releases/download/") {
			out = append(out, r)
		}
	}
	return out
}

// updateEnv is a self-update of a fake rpak binary in a temporary
// directory: never the test binary.
type updateEnv struct {
	t              *testing.T
	srv            *releaseServer
	dir, exe       string // the binary and its directory
	path           string // what os.Executable would say: exe, or a symlink to it
	stdout, stderr bytes.Buffer
}

func newUpdateEnv(t *testing.T, srv *releaseServer) *updateEnv {
	dir := filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "rpak")
	if err := os.WriteFile(exe, []byte("old rpak"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &updateEnv{t: t, srv: srv, dir: dir, exe: exe, path: exe}
}

// run runs rpak self-update ARGV as rpak version, on amd64; answer is
// what the user types on a terminal, or "-" for no terminal.
func (e *updateEnv) run(version, answer string, argv ...string) int {
	e.t.Helper()
	e.stdout.Reset()
	e.stderr.Reset()
	opts, help, usageErr := parseSelfUpdateArgs(argv)
	if help || usageErr != "" {
		e.t.Fatalf("%q: help %v, %s", argv, help, usageErr)
	}
	u := &selfUpdater{
		version:    version,
		enabled:    true,
		client:     &release.Client{Base: e.srv.URL + "/releases", HTTP: e.srv.Client(), Version: version},
		arch:       "amd64",
		executable: func() (string, error) { return e.path, nil },
		stdout:     &e.stdout,
		stderr:     &e.stderr,
	}
	if answer != "-" {
		r, w, err := os.Pipe()
		if err != nil {
			e.t.Fatal(err)
		}
		defer r.Close()
		w.WriteString(answer)
		w.Close()
		u.canAsk, u.in = true, r
	}
	return u.run(context.Background(), opts)
}

func (e *updateEnv) binary() string {
	b, err := os.ReadFile(e.exe)
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

// clean checks that nothing but the binary is in its directory.
func (e *updateEnv) clean() {
	e.t.Helper()
	entries, _ := os.ReadDir(e.dir)
	if len(entries) != 1 {
		var names []string
		for _, en := range entries {
			names = append(names, en.Name())
		}
		e.t.Errorf("in %s: %q", e.dir, names)
	}
}

func (e *updateEnv) expect(code, want int, stdout, stderr string) {
	e.t.Helper()
	if code != want || !strings.Contains(e.stdout.String(), stdout) || !strings.Contains(e.stderr.String(), stderr) {
		e.t.Errorf("exit %d (want %d)\nstdout: %q (want %q)\nstderr: %q (want %q)", code, want, e.stdout.String(), stdout, e.stderr.String(), stderr)
	}
}

func TestParseSelfUpdateArgs(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want selfUpdateOptions
	}{
		{nil, selfUpdateOptions{}},
		{[]string{"v0.3.6"}, selfUpdateOptions{version: "v0.3.6"}},
		{[]string{"0.3.6", "-y", "--force"}, selfUpdateOptions{version: "v0.3.6", yes: true, force: true}},
		{[]string{"--check", "--yes"}, selfUpdateOptions{check: true, yes: true}},
	} {
		got, help, usageErr := parseSelfUpdateArgs(c.argv)
		if got != c.want || help || usageErr != "" {
			t.Errorf("%q: got %+v, %v, %q", c.argv, got, help, usageErr)
		}
	}
	for _, argv := range [][]string{{"-h"}, {"v1", "--help"}} {
		if _, help, _ := parseSelfUpdateArgs(argv); !help {
			t.Errorf("%q: no help", argv)
		}
	}
	for argv, want := range map[string]string{
		"v0.3":          "v0.3 is not a version such as v0.3.6",
		"latest":        "latest is not a version",
		"v0.3.6-rc1":    "not a version",
		"v0.3.6 v0.3.7": "too many arguments",
		"--now":         "unknown option --now",
		"-x":            "unknown option -x",
	} {
		if _, _, usageErr := parseSelfUpdateArgs(strings.Fields(argv)); !strings.Contains(usageErr, want) {
			t.Errorf("%q: %q, want %q", argv, usageErr, want)
		}
	}
}

func TestSelfUpdate(t *testing.T) {
	srv := newReleaseServer(t, "v0.3.6", "v0.3.5", "v0.3.6")
	e := newUpdateEnv(t, srv)
	code := e.run("v0.3.5", "-", "-y")
	e.expect(code, 0, "rpak updated: v0.3.5 → v0.3.6  "+e.exe, "")
	if got := e.binary(); got != binary("v0.3.6", "amd64") {
		t.Errorf("binary: %q", got)
	}
	if fi, _ := os.Stat(e.exe); fi.Mode() != 0o755 {
		t.Errorf("mode %v", fi.Mode())
	}
	e.clean()
	if got := strings.Join(srv.downloads(), " "); got != "/releases/download/v0.3.6/SHA256SUMS /releases/download/v0.3.6/rpak-linux-amd64" {
		t.Errorf("downloads: %s", got)
	}

	// Now up to date: nothing is downloaded.
	srv.reqs = nil
	code = e.run("v0.3.6", "-", "-y")
	e.expect(code, 0, "rpak v0.3.6 is the latest release\n", "")
	if d := srv.downloads(); d != nil {
		t.Errorf("downloads: %q", d)
	}
	// Newer than the latest release (it was pulled): left alone.
	code = e.run("v0.3.7", "-", "-y")
	e.expect(code, 0, "rpak v0.3.7 is newer than the latest release v0.3.6\n", "")
	// The same, asked for: only with --force.
	code = e.run("v0.3.6", "-", "-y", "0.3.6")
	e.expect(code, 0, "rpak v0.3.6 is installed already; --force installs it again\n", "")
	if d := srv.downloads(); d != nil {
		t.Errorf("downloads: %q", d)
	}
	code = e.run("v0.3.6", "-", "-y", "--force")
	e.expect(code, 0, "rpak updated: v0.3.6 → v0.3.6", "")
}

func TestSelfUpdateSymlink(t *testing.T) {
	srv := newReleaseServer(t, "v0.3.6", "v0.3.6")
	e := newUpdateEnv(t, srv)
	// ~/.local/bin/rpak -> ../opt/rpak/bin/rpak
	link := filepath.Join(filepath.Dir(e.dir), "link", "rpak")
	os.Mkdir(filepath.Dir(link), 0o755)
	if err := os.Symlink("../bin/rpak", link); err != nil {
		t.Fatal(err)
	}
	e.path = link
	code := e.run("v0.3.5", "-", "-y")
	e.expect(code, 0, "rpak updated: v0.3.5 → v0.3.6  "+e.exe, "")
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("%s is no longer a symlink: %v, %v", link, fi.Mode(), err)
	}
	if got := e.binary(); got != binary("v0.3.6", "amd64") {
		t.Errorf("binary: %q", got)
	}
	e.clean()
	if entries, _ := os.ReadDir(filepath.Dir(link)); len(entries) != 1 {
		t.Errorf("%d files next to the symlink", len(entries))
	}
}

func TestSelfUpdateDowngrade(t *testing.T) {
	srv := newReleaseServer(t, "v0.3.6", "v0.3.5", "v0.3.6")
	e := newUpdateEnv(t, srv)
	code := e.run("v0.3.6", "\n", "v0.3.5")
	e.expect(code, 0, "rpak updated: v0.3.6 → v0.3.5", "Downgrade rpak v0.3.6 to v0.3.5? [Y/n]: ")
	if got := e.binary(); got != binary("v0.3.5", "amd64") {
		t.Errorf("binary: %q", got)
	}
	// A release that does not exist.
	code = e.run("v0.3.5", "-", "-y", "v0.2.0")
	e.expect(code, 1, "", "v0.2.0/SHA256SUMS: not found")
	e.clean()
}

func TestSelfUpdateConfirm(t *testing.T) {
	srv := newReleaseServer(t, "v0.3.6", "v0.3.6")
	for _, c := range []struct {
		answer  string
		code    int
		updated bool
	}{
		{"n\n", 1, false},
		{"no\n", 1, false},
		{"", 1, false}, // end of input
		{"\n", 0, true},
		{"Y\n", 0, true},
		{"yes\n", 0, true},
	} {
		e := newUpdateEnv(t, srv)
		code := e.run("v0.3.5", c.answer)
		e.expect(code, c.code, "", "Update rpak v0.3.5 to v0.3.6? [Y/n]: ")
		if updated := e.binary() != "old rpak"; updated != c.updated {
			t.Errorf("%q: updated %v", c.answer, updated)
		}
		e.clean()
	}

	// Not on a terminal: only with -y.
	srv.reqs = nil
	e := newUpdateEnv(t, srv)
	code := e.run("v0.3.5", "-")
	e.expect(code, 1, "", "rpak self-update: not on a terminal; add -y to update rpak v0.3.5 to v0.3.6\n")
	if e.binary() != "old rpak" || srv.downloads() != nil {
		t.Errorf("updated anyway: %q", srv.downloads())
	}
}

func TestSelfUpdateDevelopmentBuild(t *testing.T) {
	srv := newReleaseServer(t, "v0.3.6", "v0.3.6")
	for _, version := range []string{"dev", "v0.3.5-dirty", "v0.3.5-3-gabc1234", "v0.3.6-1-gabc1234", "abc1234"} {
		srv.reqs = nil
		e := newUpdateEnv(t, srv)
		code := e.run(version, "-", "-y")
		e.expect(code, 1, "", "rpak self-update: rpak "+version+" is a development build, not a release; --force replaces it with one\n")
		if e.binary() != "old rpak" || srv.requests() != nil {
			t.Errorf("%s: updated anyway: %q", version, srv.requests())
		}

		code = e.run(version, "-", "-y", "--force")
		e.expect(code, 0, "rpak updated: "+version+" → v0.3.6", "")
		if got := e.binary(); got != binary("v0.3.6", "amd64") {
			t.Errorf("%s --force: binary %q", version, got)
		}
	}
	e := newUpdateEnv(t, srv)
	code := e.run("v0.3.5-dirty", "\n", "--force")
	e.expect(code, 0, "rpak updated", "Replace rpak v0.3.5-dirty with v0.3.6? [Y/n]: ")
}

func TestSelfUpdateCheck(t *testing.T) {
	srv := newReleaseServer(t, "v0.3.6", "v0.3.5", "v0.3.6")
	for _, c := range []struct {
		version string
		argv    []string
		code    int
		stdout  string
	}{
		{"v0.3.5", nil, exitNewer, "rpak v0.3.5; the latest release v0.3.6 is newer: rpak self-update\n"},
		{"v0.3.6", nil, 0, "rpak v0.3.6 is the latest release\n"},
		{"v0.3.7", nil, 0, "rpak v0.3.7 is newer than the latest release v0.3.6\n"},
		{"v0.3.5", []string{"v0.3.6"}, exitNewer, "rpak v0.3.5; release v0.3.6 is newer: rpak self-update v0.3.6\n"},
		{"v0.3.6", []string{"v0.3.5"}, 0, "rpak v0.3.6 is newer than release v0.3.5\n"},
		// Development builds are not refused: --check changes nothing.
		{"v0.3.5-3-gabc1234", nil, exitNewer, "rpak v0.3.5-3-gabc1234 is a development build; the latest release v0.3.6\n"},
		{"v0.3.6-3-gabc1234", nil, 0, "rpak v0.3.6-3-gabc1234 is a development build; the latest release v0.3.6\n"},
		{"v0.3.6-dirty", nil, 0, "is a development build"},
		{"dev", nil, 0, "rpak dev is a development build; the latest release v0.3.6\n"},
	} {
		e := newUpdateEnv(t, srv)
		code := e.run(c.version, "-", append([]string{"--check"}, c.argv...)...)
		if code != c.code || !strings.Contains(e.stdout.String(), c.stdout) || e.stderr.Len() != 0 {
			t.Errorf("%s --check %q: exit %d, stdout %q, stderr %q; want %d, %q", c.version, c.argv, code, e.stdout.String(), e.stderr.String(), c.code, c.stdout)
		}
		if e.binary() != "old rpak" {
			t.Errorf("%s --check: updated", c.version)
		}
	}
	if d := srv.downloads(); d != nil {
		t.Errorf("downloads: %q", d)
	}

	// No answer: 1.
	srv.Close()
	e := newUpdateEnv(t, srv)
	code := e.run("v0.3.5", "-", "--check")
	e.expect(code, 1, "", "rpak self-update: cannot find the latest release: ")
}

func TestSelfUpdateChecksum(t *testing.T) {
	srv := newReleaseServer(t, "v0.3.6", "v0.3.6")
	for name, sums := range map[string]string{
		"mismatch":     sha("something else") + "  rpak-linux-amd64\n",
		"missing line": sha(binary("v0.3.6", "arm64")) + "  rpak-linux-arm64\n",
	} {
		srv.mu.Lock()
		srv.sums["v0.3.6"] = sums
		srv.mu.Unlock()
		e := newUpdateEnv(t, srv)
		code := e.run("v0.3.5", "-", "-y")
		want := "has no checksum of rpak-linux-amd64"
		if name == "mismatch" {
			want = "checksum mismatch: " + srv.URL + "/releases/download/v0.3.6/rpak-linux-amd64 has sha256 " + sha(binary("v0.3.6", "amd64")) + ", SHA256SUMS says " + sha("something else") + "; " + e.exe + " is unchanged\n"
		}
		e.expect(code, 1, "", want)
		if e.binary() != "old rpak" {
			t.Errorf("%s: updated", name)
		}
		e.clean()
	}
}

func TestSelfUpdateNotWritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	srv := newReleaseServer(t, "v0.3.6", "v0.3.6")
	e := newUpdateEnv(t, srv)
	if err := os.Chmod(e.dir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(e.dir, 0o755)
	for _, answer := range []string{"-", "\n"} {
		code := e.run("v0.3.5", answer)
		e.expect(code, 1, "", "rpak self-update: cannot write "+e.dir+"; run with sudo, or reinstall\n")
		// Refused before asking, and before downloading.
		if strings.Contains(e.stderr.String(), "[Y/n]") {
			t.Error("asked")
		}
		if d := srv.downloads(); d != nil {
			t.Errorf("downloads: %q", d)
		}
	}
	if e.binary() != "old rpak" {
		t.Error("updated")
	}
}

func TestSelfUpdateDisabled(t *testing.T) {
	srv := newReleaseServer(t, "v0.3.6", "v0.3.6")
	e := newUpdateEnv(t, srv)
	for _, argv := range [][]string{{"-y"}, {"--check"}, {"--force", "-y", "v0.3.6"}} {
		var stdout, stderr bytes.Buffer
		u := &selfUpdater{
			version:    "v0.3.5",
			client:     &release.Client{Base: srv.URL + "/releases", HTTP: srv.Client()},
			arch:       "amd64",
			executable: func() (string, error) { return e.exe, nil },
			stdout:     &stdout,
			stderr:     &stderr,
		}
		opts, _, _ := parseSelfUpdateArgs(argv)
		code := u.run(context.Background(), opts)
		if code != 1 || stdout.Len() != 0 || stderr.String() != "rpak self-update: rpak was installed by a package manager; update it with that\n" {
			t.Errorf("%q: exit %d, stdout %q, stderr %q", argv, code, stdout.String(), stderr.String())
		}
	}
	if srv.requests() != nil || e.binary() != "old rpak" {
		t.Errorf("requests: %q", srv.requests())
	}
}

func TestSelfUpdateCommandUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	for _, enabled := range []bool{true, false} {
		stdout.Reset()
		if code := selfUpdateCommand("v0.3.5", enabled, []string{"--help"}, &stdout, &stderr); code != 0 || !strings.HasPrefix(stdout.String(), "Usage: rpak self-update [VERSION]") {
			t.Errorf("--help: exit %d, %q", code, stdout.String())
		}
	}
	stdout.Reset()
	if code := selfUpdateCommand("v0.3.5", true, []string{"--yes", "--bogus"}, &stdout, &stderr); code != 2 || stdout.Len() != 0 ||
		stderr.String() != "rpak self-update: unknown option --bogus\nSee \"rpak self-update --help\".\n" {
		t.Errorf("--bogus: exit %d, stderr %q", code, stderr.String())
	}
}
