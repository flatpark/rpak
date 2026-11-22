package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestParseTag(t *testing.T) {
	for s, want := range map[string]Version{
		"v0.3.6":     {0, 3, 6, 0, false},
		"v1.0.0":     {1, 0, 0, 0, false},
		"v10.20.300": {10, 20, 300, 0, false},
	} {
		if got, ok := ParseTag(s); !ok || got != want {
			t.Errorf("%s: got %+v, %v", s, got, ok)
		}
	}
	for _, s := range []string{"", "v", "0.3.6", "v0.3", "v0.3.6.1", "v0.3.6-rc1", "v0.3.6-dirty", "v01.2.3", "v0.3.x", "v0..6", "V0.3.6", "v-1.2.3", "v0.3.6 "} {
		if _, ok := ParseTag(s); ok {
			t.Errorf("%q: parsed", s)
		}
	}
}

func TestParseBuild(t *testing.T) {
	for s, want := range map[string]Version{
		"v0.3.5":                   {0, 3, 5, 0, false},
		"v0.3.5-3-gabc1234":        {0, 3, 5, 3, false},
		"v0.3.5-dirty":             {0, 3, 5, 0, true},
		"v0.3.5-12-gabc1234-dirty": {0, 3, 5, 12, true},
	} {
		got, ok := ParseBuild(s)
		if !ok || got != want {
			t.Errorf("%s: got %+v, %v", s, got, ok)
		}
		if dev := s != "v0.3.5"; got.Dev() != dev {
			t.Errorf("%s: Dev() = %v", s, got.Dev())
		}
	}
	for _, s := range []string{"dev", "abc1234", "abc1234-dirty", "v0.3.5-3", "v0.3.5-3-g", "v0.3.5-x-gabc", "v0.3.5-0-gabc", "v0.3.5-3-gXYZ", "v0.3.5-rc1"} {
		if _, ok := ParseBuild(s); ok {
			t.Errorf("%q: parsed", s)
		}
	}
}

func TestCompare(t *testing.T) {
	build := func(s string) Version {
		v, ok := ParseBuild(s)
		if !ok {
			t.Fatalf("%s: not parsed", s)
		}
		return v
	}
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"v0.3.6", "v0.3.5", 1},
		{"v0.3.5", "v0.3.6", -1},
		{"v0.3.5", "v0.3.5", 0},
		{"v0.10.0", "v0.9.9", 1},
		{"v1.0.0", "v0.99.99", 1},
		// Built after the tag: newer than it, older than the next.
		{"v0.3.5-3-gabc1234", "v0.3.5", 1},
		{"v0.3.5-3-gabc1234", "v0.3.6", -1},
		{"v0.3.5-10-gabc1234", "v0.3.5-9-gdef", 1},
		// -dirty does not count.
		{"v0.3.5-dirty", "v0.3.5", 0},
		{"v0.3.5-dirty", "v0.3.6", -1},
	} {
		if got := Compare(build(c.a), build(c.b)); got != c.want {
			t.Errorf("Compare(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
		if got := Compare(build(c.b), build(c.a)); got != -c.want {
			t.Errorf("Compare(%s, %s) = %d, want %d", c.b, c.a, got, -c.want)
		}
	}
	if got := build("v0.3.5-3-gabc1234-dirty").Tag(); got != "v0.3.5" {
		t.Errorf("Tag() = %s", got)
	}
}

func TestBase(t *testing.T) {
	env := func(v string) func(string) (string, bool) {
		return func(name string) (string, bool) {
			if name != BaseEnv {
				t.Errorf("looked up %s", name)
			}
			return v, v != ""
		}
	}
	if got := Base(env("")); got != DefaultBase {
		t.Errorf("unset: %s", got)
	}
	if got := Base(env("http://mirror.example/rpak/releases/")); got != "http://mirror.example/rpak/releases" {
		t.Errorf("set: %s", got)
	}
}

// server is a release server shaped like GitHub's: /releases/latest
// redirects to /releases/tag/LATEST, and /releases/download/TAG/NAME are
// the files. It records the requests.
type server struct {
	*httptest.Server
	mu     sync.Mutex
	latest func(w http.ResponseWriter, r *http.Request)
	files  map[string][]byte // by path
	reqs   []string          // "METHOD PATH"
	agents []string
}

func newServer(t *testing.T, latest string) *server {
	s := &server{files: map[string][]byte{}}
	s.latest = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/releases/tag/"+latest, http.StatusFound)
	}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.reqs = append(s.reqs, r.Method+" "+r.URL.Path)
		s.agents = append(s.agents, r.UserAgent())
		latest, content, ok := s.latest, s.files[r.URL.Path], s.files[r.URL.Path] != nil
		s.mu.Unlock()
		switch {
		case r.URL.Path == "/releases/latest":
			latest(w, r)
		case ok:
			w.Write(content)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *server) client() *Client {
	return &Client{Base: s.URL + "/releases", HTTP: s.Client(), Version: "test"}
}

// release adds the files of a release: the binary of each name, and
// SHA256SUMS, made of sums if given, else of the binaries.
func (s *server) release(tag string, binaries map[string]string, sums string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	for name, content := range binaries {
		s.files["/releases/download/"+tag+"/"+name] = []byte(content)
		h := sha256.Sum256([]byte(content))
		fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(h[:]), name)
	}
	if sums == "" {
		sums = b.String()
	}
	s.files["/releases/download/"+tag+"/SHA256SUMS"] = []byte(sums)
}

func (s *server) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.reqs...)
}

func TestLatest(t *testing.T) {
	s := newServer(t, "v0.3.6")
	tag, err := s.client().Latest(context.Background())
	if err != nil || tag != "v0.3.6" {
		t.Fatalf("got %q, %v", tag, err)
	}
	// One HEAD, and the redirect is not followed.
	if got := strings.Join(s.requests(), ", "); got != "HEAD /releases/latest" {
		t.Errorf("requests: %s", got)
	}
	if s.agents[0] != "rpak/test" {
		t.Errorf("User-Agent %q", s.agents[0])
	}

	// GitHub's answer, an absolute URL.
	s.latest = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://github.com/flatpark/rpak/releases/tag/v0.10.2")
		w.WriteHeader(http.StatusFound)
	}
	if tag, err := s.client().Latest(context.Background()); err != nil || tag != "v0.10.2" {
		t.Errorf("absolute: got %q, %v", tag, err)
	}

	for name, c := range map[string]struct {
		status   int
		location string
		want     string
	}{
		"200":         {http.StatusOK, "", "HTTP 200 from " + s.URL + "/releases/latest, not a redirect"},
		"404":         {http.StatusNotFound, "", "HTTP 404"},
		"no location": {http.StatusFound, "", "redirects nowhere"},
		// GitHub, for a repository without releases.
		"no release": {http.StatusFound, "/flatpark/rpak/releases", "not to a release"},
		"not semver": {http.StatusFound, "/releases/tag/nightly", `"nightly", not a version`},
		"prerelease": {http.StatusFound, "/releases/tag/v0.4.0-rc1", "not a version"},
	} {
		s.latest = func(w http.ResponseWriter, r *http.Request) {
			if c.location != "" {
				w.Header().Set("Location", c.location)
			}
			w.WriteHeader(c.status)
		}
		tag, err := s.client().Latest(context.Background())
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %q, %v; want an error with %q", name, tag, err, c.want)
		}
		if IsNetwork(err) {
			t.Errorf("%s: %v is a network error", name, err)
		}
	}

	// No answer at all.
	s.Close()
	if _, err := s.client().Latest(context.Background()); err == nil || !IsNetwork(err) {
		t.Errorf("closed server: %v", err)
	}
}

func TestChecksum(t *testing.T) {
	s := newServer(t, "v0.3.6")
	a := strings.Repeat("a", 64)
	b := strings.Repeat("B", 64)
	s.release("v0.3.6", nil, a+"  rpak-linux-amd64\n"+b+" *rpak-linux-arm64\nshort  rpak-linux-x86_64\n")
	c := s.client()
	if sum, err := c.Checksum(context.Background(), "v0.3.6", "rpak-linux-amd64"); err != nil || sum != a {
		t.Errorf("amd64: %q, %v", sum, err)
	}
	if sum, err := c.Checksum(context.Background(), "v0.3.6", "rpak-linux-arm64"); err != nil || sum != strings.ToLower(b) {
		t.Errorf("arm64 (binary mode): %q, %v", sum, err)
	}
	for _, name := range []string{"rpak-linux-x86_64", "rpak-linux-riscv64", "rpak-linux"} {
		if sum, err := c.Checksum(context.Background(), "v0.3.6", name); err == nil || !strings.Contains(err.Error(), "has no checksum of "+name) {
			t.Errorf("%s: %q, %v", name, sum, err)
		}
	}
	if _, err := c.Checksum(context.Background(), "v9.9.9", "rpak-linux-amd64"); err == nil || !strings.Contains(err.Error(), "v9.9.9/SHA256SUMS: not found") {
		t.Errorf("missing release: %v", err)
	}
}

// leftovers are the files in dir other than those named.
func leftovers(t *testing.T, dir string, names ...string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
outer:
	for _, e := range entries {
		for _, n := range names {
			if e.Name() == n {
				continue outer
			}
		}
		out = append(out, e.Name())
	}
	return out
}

func TestInstall(t *testing.T) {
	s := newServer(t, "v0.3.6")
	s.release("v0.3.6", map[string]string{"rpak-linux-amd64": "new amd64", "rpak-linux-arm64": "new arm64"}, "")
	dir := t.TempDir()
	target := filepath.Join(dir, "rpak")
	if err := os.WriteFile(target, []byte("old"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.client().Install(context.Background(), "v0.3.6", "rpak-linux-arm64", target); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "new arm64" {
		t.Errorf("target: %q", b)
	}
	if fi, _ := os.Stat(target); fi.Mode() != 0o755 {
		t.Errorf("mode %v", fi.Mode())
	}
	if l := leftovers(t, dir, "rpak"); l != nil {
		t.Errorf("left behind: %q", l)
	}
}

func TestInstallFails(t *testing.T) {
	good := strings.Repeat("0", 64) + "  rpak-linux-amd64\n"
	for name, c := range map[string]struct {
		sums     string // SHA256SUMS; "" for the right one
		binary   bool   // the binary is there
		want     string
		download bool // the binary is asked for
	}{
		"mismatch":     {sums: good, binary: true, want: "checksum mismatch", download: true},
		"missing line": {sums: strings.Repeat("0", 64) + "  rpak-linux-arm64\n", binary: true, want: "has no checksum of rpak-linux-amd64"},
		"no sums":      {sums: "-", binary: true, want: "SHA256SUMS: not found"},
		"no binary":    {binary: false, sums: good, want: "rpak-linux-amd64: not found", download: true},
	} {
		t.Run(name, func(t *testing.T) {
			s := newServer(t, "v0.3.6")
			binaries := map[string]string{"rpak-linux-amd64": "new"}
			s.release("v0.3.6", binaries, c.sums)
			s.mu.Lock()
			if !c.binary {
				delete(s.files, "/releases/download/v0.3.6/rpak-linux-amd64")
			}
			if c.sums == "-" {
				delete(s.files, "/releases/download/v0.3.6/SHA256SUMS")
			}
			s.mu.Unlock()
			dir := t.TempDir()
			target := filepath.Join(dir, "rpak")
			if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
				t.Fatal(err)
			}
			err := s.client().Install(context.Background(), "v0.3.6", "rpak-linux-amd64", target)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error with %q", err, c.want)
			}
			if name == "mismatch" && !errors.Is(err, ErrChecksum) {
				t.Errorf("%v is not ErrChecksum", err)
			}
			if b, _ := os.ReadFile(target); string(b) != "old" {
				t.Errorf("target changed: %q", b)
			}
			if l := leftovers(t, dir, "rpak"); l != nil {
				t.Errorf("left behind: %q", l)
			}
			downloaded := false
			for _, r := range s.requests() {
				downloaded = downloaded || strings.HasSuffix(r, "/rpak-linux-amd64")
			}
			if downloaded != c.download {
				t.Errorf("binary asked for: %v, requests %q", downloaded, s.requests())
			}
		})
	}
}

func TestInstallCanceled(t *testing.T) {
	s := newServer(t, "v0.3.6")
	s.release("v0.3.6", map[string]string{"rpak-linux-amd64": "new"}, "")
	// The download stops halfway, as on Ctrl-C.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	block := make(chan struct{})
	s.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "SHA256SUMS") {
			h := sha256.Sum256([]byte("new"))
			fmt.Fprintf(w, "%s  rpak-linux-amd64\n", hex.EncodeToString(h[:]))
			return
		}
		w.Header().Set("Content-Length", "1000")
		w.Write([]byte("ne"))
		w.(http.Flusher).Flush()
		cancel()
		<-block
	})
	defer close(block)
	dir := t.TempDir()
	target := filepath.Join(dir, "rpak")
	os.WriteFile(target, []byte("old"), 0o755)
	if err := s.client().Install(ctx, "v0.3.6", "rpak-linux-amd64", target); err == nil {
		t.Error("no error")
	}
	if l := leftovers(t, dir, "rpak"); l != nil {
		t.Errorf("left behind: %q", l)
	}
}

func TestWritable(t *testing.T) {
	dir := t.TempDir()
	if err := Writable(dir); err != nil {
		t.Errorf("writable: %v", err)
	}
	if l := leftovers(t, dir); l != nil {
		t.Errorf("left behind: %q", l)
	}
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o755)
	if err := Writable(dir); err == nil {
		t.Error("read-only directory: no error")
	}
}
