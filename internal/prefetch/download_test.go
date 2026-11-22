package prefetch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flatpark/rpak/internal/metadata"
)

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func payload(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i*7)
	}
	return b
}

// server serves files by path and counts requests.
type server struct {
	*httptest.Server
	files    map[string][]byte
	requests atomic.Int64
	ranges   atomic.Int64 // requests with a Range header
	handler  func(w http.ResponseWriter, r *http.Request, content []byte) bool
	ua       atomic.Value
}

func newServer(t *testing.T) *server {
	s := &server{files: make(map[string][]byte)}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		s.ua.Store(r.UserAgent())
		if r.Header.Get("Range") != "" {
			s.ranges.Add(1)
		}
		content, ok := s.files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if s.handler != nil && s.handler(w, r, content) {
			return
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(content))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *server) add(path string, content []byte) metadata.Source {
	s.files[path] = content
	return metadata.Source{
		Ref:      "app/org.example.App/x86_64/stable",
		Name:     filepath.Base(path),
		Checksum: sum(content),
		Size:     int64(len(content)),
		URI:      s.URL + path,
	}
}

func testConfig(t *testing.T, base string) Config {
	cfg := NewConfig(base, "test")
	cfg.Backoff = time.Millisecond
	cfg.Retries = 0
	cfg.FreeSpace = func(string) (uint64, error) { return 1 << 40, nil }
	return cfg
}

func run(t *testing.T, cfg Config, sources ...metadata.Source) (Summary, map[string]Result) {
	t.Helper()
	var mu sync.Mutex
	results := make(map[string]Result)
	cfg.OnDone = func(s metadata.Source, r Result) {
		mu.Lock()
		results[s.Name] = r
		mu.Unlock()
	}
	return New(cfg).DownloadAll(context.Background(), sources), results
}

func target(base string, s metadata.Source) string {
	return filepath.Join(ExtraDataDir(base, s.Checksum), s.Name)
}

func assertFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s: wrong content (%d bytes, want %d)", path, len(got), len(want))
	}
}

func assertMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s exists (err %v)", path, err)
	}
}

func TestDownload(t *testing.T) {
	srv := newServer(t)
	base := t.TempDir()
	content := payload(300_000, 1)
	s := srv.add("/pool/app.deb", content)
	var progress atomic.Int64
	cfg := testConfig(t, base)
	cfg.OnProgress = func(_ metadata.Source, n int64) { progress.Add(n) }
	summary, results := run(t, cfg, s)

	if summary.Downloaded != 1 || results["app.deb"].Status != Downloaded {
		t.Fatalf("summary %+v, results %+v", summary, results)
	}
	assertFile(t, target(base, s), content)
	assertMissing(t, target(base, s)+".part")
	dir := ExtraDataDir(base, s.Checksum)
	if !hasMarker(dir) || len(summary.Dirs) != 1 || summary.Dirs[0] != dir {
		t.Fatalf("marker / dirs wrong: %+v", summary)
	}
	if progress.Load() != int64(len(content)) {
		t.Errorf("progress = %d", progress.Load())
	}
	if ua := srv.ua.Load(); ua != "rpak/test" {
		t.Errorf("User-Agent = %v", ua)
	}
	// Nothing else is left in extra-data/, in particular no temporary dirs.
	entries, _ := os.ReadDir(filepath.Join(base, "extra-data"))
	if len(entries) != 1 {
		t.Errorf("extra-data has %d entries", len(entries))
	}
}

// A whole file that is not the source's is upstream's new one: it is not
// retried, nothing is left under the source's checksum (flatpak would fail
// on it), and the file is kept under its own for an extra-data patch.
func TestChangedFile(t *testing.T) {
	for _, tc := range []struct {
		name   string
		served []byte
	}{
		{"same size", payload(5000, 9)},
		{"longer", payload(6000, 2)},
		{"shorter", payload(4000, 2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newServer(t)
			base := t.TempDir()
			want := payload(5000, 2)
			s := srv.add("/a.bin", tc.served)
			s.Size, s.Checksum = int64(len(want)), sum(want)
			cfg := testConfig(t, base)
			cfg.Retries = 2
			summary, results := run(t, cfg, s)

			r := results["a.bin"]
			if r.Status != Changed || len(summary.Changed) != 1 || len(summary.Dirs) != 0 {
				t.Fatalf("summary %+v, results %+v", summary, results)
			}
			if srv.requests.Load() != 1 {
				t.Errorf("requests = %d, want 1: a changed file is not retried", srv.requests.Load())
			}
			got := summary.Changed[0].Got
			if got.Size != int64(len(tc.served)) || got.Checksum != sum(tc.served) || got.URI != s.URI || got.Ref != s.Ref {
				t.Errorf("got %+v", got)
			}
			assertMissing(t, target(base, s))
			assertMissing(t, target(base, s)+".part")
			if r.Dir != ExtraDataDir(base, got.Checksum) || !hasMarker(r.Dir) {
				t.Errorf("dir %q", r.Dir)
			}
			assertFile(t, target(base, got), tc.served)
			if !HasFile(r.Dir, got) {
				t.Error("HasFile is false")
			}
		})
	}
}

// Without a length or chunked framing, a short body may be a dropped
// connection: it is resumed, not taken as a new file.
func TestShortUnframedBodyIsResumed(t *testing.T) {
	srv := newServer(t)
	base := t.TempDir()
	content := payload(50_000, 4)
	s := srv.add("/a.bin", content)
	var calls atomic.Int64
	srv.handler = func(w http.ResponseWriter, r *http.Request, content []byte) bool {
		if calls.Add(1) > 1 {
			return false
		}
		hj, _ := w.(http.Hijacker)
		conn, buf, _ := hj.Hijack()
		defer conn.Close()
		fmt.Fprintf(buf, "HTTP/1.0 200 OK\r\nContent-Type: application/octet-stream\r\n\r\n")
		buf.Write(content[:20_000])
		buf.Flush()
		return true
	}
	cfg := testConfig(t, base)
	cfg.Retries = 1
	summary, results := run(t, cfg, s)
	if summary.Downloaded != 1 || srv.ranges.Load() != 1 {
		t.Fatalf("summary %+v, results %+v, ranges %d", summary, results, srv.ranges.Load())
	}
	assertFile(t, target(base, s), content)
}

// A resumed download that ends up wrong is started over: its start may be
// of the old file.
func TestResumedMismatchStartsOver(t *testing.T) {
	srv := newServer(t)
	base := t.TempDir()
	served := payload(5000, 7)
	want := payload(5000, 8)
	s := srv.add("/a.bin", served)
	s.Checksum = sum(want)
	dir := ExtraDataDir(base, s.Checksum)
	if ok, err := claimDir(dir, "old\n"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if err := os.WriteFile(target(base, s)+".part", want[:1000], 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, base)
	cfg.Retries = 1
	summary, results := run(t, cfg, s)
	if len(summary.Changed) != 1 || srv.requests.Load() != 2 || srv.ranges.Load() != 1 {
		t.Fatalf("summary %+v, results %+v, requests %d", summary, results, srv.requests.Load())
	}
	if got := summary.Changed[0].Got; got.Checksum != sum(served) {
		t.Errorf("got %+v, want the whole served file", got)
	}
}

// A resume whose Content-Range says another total starts over.
func TestResumeOtherTotalStartsOver(t *testing.T) {
	srv := newServer(t)
	base := t.TempDir()
	served := payload(6000, 7)
	want := payload(5000, 7)
	s := srv.add("/a.bin", served)
	s.Size, s.Checksum = int64(len(want)), sum(want)
	if ok, err := claimDir(ExtraDataDir(base, s.Checksum), "old\n"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if err := os.WriteFile(target(base, s)+".part", want[:1000], 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, base)
	cfg.Retries = 1
	summary, _ := run(t, cfg, s)
	if len(summary.Changed) != 1 || summary.Changed[0].Got.Checksum != sum(served) || srv.requests.Load() != 2 {
		t.Fatalf("summary %+v, requests %d", summary, srv.requests.Load())
	}
}

func TestNotFoundIsNotRetried(t *testing.T) {
	srv := newServer(t)
	base := t.TempDir()
	s := srv.add("/a.bin", payload(100, 1))
	s.URI = srv.URL + "/gone.bin"
	cfg := testConfig(t, base)
	cfg.Retries = 3
	var retries atomic.Int64
	cfg.OnRetry = func(metadata.Source, error, time.Duration) { retries.Add(1) }
	summary, _ := run(t, cfg, s)
	if summary.Failed != 1 || srv.requests.Load() != 1 || retries.Load() != 0 {
		t.Fatalf("summary %+v, requests %d, retries %d", summary, srv.requests.Load(), retries.Load())
	}
}

func TestRetryBackoff(t *testing.T) {
	srv := newServer(t)
	base := t.TempDir()
	content := payload(1000, 1)
	s := srv.add("/a.bin", content)
	var calls atomic.Int64
	srv.handler = func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if calls.Add(1) <= 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return true
		}
		return false
	}
	cfg := testConfig(t, base)
	cfg.Retries = 4
	cfg.Backoff, cfg.MaxBackoff = time.Millisecond, 3*time.Millisecond
	var mu sync.Mutex
	var waits []time.Duration
	cfg.OnRetry = func(_ metadata.Source, err error, wait time.Duration) {
		mu.Lock()
		waits = append(waits, wait)
		mu.Unlock()
	}
	summary, _ := run(t, cfg, s)
	if summary.Downloaded != 1 {
		t.Fatalf("summary %+v", summary)
	}
	want := []time.Duration{time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond}
	if fmt.Sprint(waits) != fmt.Sprint(want) {
		t.Errorf("waits %v, want %v", waits, want)
	}
}

func TestResumeWithRange(t *testing.T) {
	srv := newServer(t)
	base := t.TempDir()
	content := payload(200_000, 3)
	s := srv.add("/big.bin", content)
	dir := ExtraDataDir(base, s.Checksum)
	if ok, err := claimDir(dir, "old\n"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if err := os.WriteFile(target(base, s)+".part", content[:123_456], 0o644); err != nil {
		t.Fatal(err)
	}
	var progress atomic.Int64
	cfg := testConfig(t, base)
	cfg.OnProgress = func(_ metadata.Source, n int64) { progress.Add(n) }
	summary, _ := run(t, cfg, s)

	if summary.Downloaded != 1 || srv.ranges.Load() != 1 {
		t.Fatalf("summary %+v, range requests %d", summary, srv.ranges.Load())
	}
	if progress.Load() != int64(len(content)-123_456) {
		t.Errorf("transferred %d bytes, want only the rest", progress.Load())
	}
	assertFile(t, target(base, s), content)
}

func TestResumeServerIgnoresRange(t *testing.T) {
	srv := newServer(t)
	base := t.TempDir()
	content := payload(200_000, 4)
	s := srv.add("/big.bin", content)
	srv.handler = func(w http.ResponseWriter, r *http.Request, content []byte) bool {
		w.WriteHeader(http.StatusOK) // always the whole file
		w.Write(content)
		return true
	}
	if ok, err := claimDir(ExtraDataDir(base, s.Checksum), "old\n"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	// A stale partial file with garbage: must be discarded, not appended to.
	if err := os.WriteFile(target(base, s)+".part", payload(50_000, 99), 0o644); err != nil {
		t.Fatal(err)
	}
	summary, _ := run(t, testConfig(t, base), s)

	if summary.Downloaded != 1 || srv.ranges.Load() != 1 {
		t.Fatalf("summary %+v, range requests %d", summary, srv.ranges.Load())
	}
	assertFile(t, target(base, s), content)
}

func TestCompletePartIsVerifiedWithoutRequest(t *testing.T) {
	srv := newServer(t)
	base := t.TempDir()
	content := payload(10_000, 5)
	s := srv.add("/x.bin", content)
	claimDir(ExtraDataDir(base, s.Checksum), "old\n")
	os.WriteFile(target(base, s)+".part", content, 0o644)
	summary, _ := run(t, testConfig(t, base), s)
	if summary.Downloaded != 1 || srv.requests.Load() != 0 {
		t.Fatalf("summary %+v, requests %d", summary, srv.requests.Load())
	}
	assertFile(t, target(base, s), content)
}

func TestForeignFileIsNotTouched(t *testing.T) {
	srv := newServer(t)
	base := t.TempDir()
	s := srv.add("/x.bin", payload(10_000, 6))
	dir := ExtraDataDir(base, s.Checksum)
	os.MkdirAll(dir, 0o755)
	foreign := []byte("placed by the user, even if wrong")
	os.WriteFile(target(base, s), foreign, 0o644)
	summary, results := run(t, testConfig(t, base), s)

	if summary.Skipped != 1 || results["x.bin"].Status != Skipped || srv.requests.Load() != 0 {
		t.Fatalf("summary %+v, requests %d", summary, srv.requests.Load())
	}
	assertFile(t, target(base, s), foreign)
	assertMissing(t, filepath.Join(dir, MarkerName))
	assertMissing(t, target(base, s)+".part")
	if len(summary.Dirs) != 0 {
		t.Errorf("dirs = %q", summary.Dirs)
	}
}

func TestUnmarkedDirectoryIsNotWrittenTo(t *testing.T) {
	srv := newServer(t)
	base := t.TempDir()
	s := srv.add("/x.bin", payload(10_000, 6))
	dir := ExtraDataDir(base, s.Checksum)
	os.MkdirAll(dir, 0o755)
	summary, _ := run(t, testConfig(t, base), s)
	if summary.Skipped != 1 || srv.requests.Load() != 0 {
		t.Fatalf("summary %+v", summary)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("directory was written to: %v", entries)
	}
}

func TestMarkedFileIsReusedWithoutRequest(t *testing.T) {
	srv := newServer(t)
	base := t.TempDir()
	content := payload(10_000, 7)
	s := srv.add("/x.bin", content)
	dir := ExtraDataDir(base, s.Checksum)
	claimDir(dir, "old\n")
	os.WriteFile(target(base, s), content, 0o644)
	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(filepath.Join(dir, MarkerName), old, old)
	summary, results := run(t, testConfig(t, base), s)

	if summary.Reused != 1 || results["x.bin"].Status != Reused || srv.requests.Load() != 0 {
		t.Fatalf("summary %+v, requests %d", summary, srv.requests.Load())
	}
	if len(summary.Dirs) != 1 {
		t.Errorf("reused dir not reported for cleanup: %+v", summary)
	}
	// Using the directory restarts its expiry clock.
	if info, _ := os.Stat(filepath.Join(dir, MarkerName)); time.Since(info.ModTime()) > time.Minute {
		t.Errorf("marker mtime not refreshed: %v", info.ModTime())
	}
}

func TestMarkedBadFileIsReplaced(t *testing.T) {
	srv := newServer(t)
	base := t.TempDir()
	content := payload(10_000, 8)
	s := srv.add("/x.bin", content)
	claimDir(ExtraDataDir(base, s.Checksum), "old\n")
	os.WriteFile(target(base, s), content[:5000], 0o644)
	summary, _ := run(t, testConfig(t, base), s)
	if summary.Downloaded != 1 || srv.requests.Load() != 1 {
		t.Fatalf("summary %+v, requests %d", summary, srv.requests.Load())
	}
	assertFile(t, target(base, s), content)
}

func TestSameChecksumDifferentNames(t *testing.T) {
	srv := newServer(t)
	base := t.TempDir()
	content := payload(10_000, 9)
	a := srv.add("/a.bin", content)
	b := a
	b.Name = "b.bin"
	summary, _ := run(t, testConfig(t, base), a, b)
	if summary.Downloaded != 2 || srv.requests.Load() != 1 || len(summary.Dirs) != 1 {
		t.Fatalf("summary %+v, requests %d", summary, srv.requests.Load())
	}
	assertFile(t, target(base, a), content)
	assertFile(t, target(base, b), content)
}

func TestHTTPErrorAndRetry(t *testing.T) {
	srv := newServer(t)
	base := t.TempDir()
	content := payload(10_000, 10)
	s := srv.add("/x.bin", content)
	var calls atomic.Int64
	srv.handler = func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if calls.Add(1) <= 2 {
			http.Error(w, "busy", http.StatusInternalServerError)
			return true
		}
		return false
	}
	cfg := testConfig(t, base)
	cfg.Retries = 2
	summary, _ := run(t, cfg, s)
	if summary.Downloaded != 1 || calls.Load() != 3 {
		t.Fatalf("summary %+v, calls %d", summary, calls.Load())
	}

	// Out of retries: failed, no file.
	calls.Store(0)
	base = t.TempDir()
	cfg = testConfig(t, base)
	cfg.Retries = 1
	summary, results := run(t, cfg, s)
	if summary.Failed != 1 || !strings.Contains(results["x.bin"].Err.Error(), "500") {
		t.Fatalf("summary %+v, results %+v", summary, results)
	}
	assertMissing(t, target(base, s))
}

func TestIdleTimeout(t *testing.T) {
	srv := newServer(t)
	base := t.TempDir()
	content := payload(100_000, 11)
	s := srv.add("/x.bin", content)
	release := make(chan struct{})
	defer close(release)
	srv.handler = func(w http.ResponseWriter, r *http.Request, content []byte) bool {
		w.Header().Set("Content-Length", fmt.Sprint(len(content)))
		w.Write(content[:40_000])
		w.(http.Flusher).Flush()
		<-release // then stall
		return true
	}
	cfg := testConfig(t, base)
	cfg.IdleTimeout = 200 * time.Millisecond
	start := time.Now()
	summary, results := run(t, cfg, s)
	if summary.Failed != 1 || !strings.Contains(results["x.bin"].Err.Error(), "no data received") {
		t.Fatalf("summary %+v, results %+v", summary, results)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
	assertMissing(t, target(base, s))
	// The partial download is kept for resuming.
	if info, err := os.Stat(target(base, s) + ".part"); err != nil || info.Size() != 40_000 {
		t.Errorf(".part: %v %v", info, err)
	}
}

func TestCancelKeepsPart(t *testing.T) {
	srv := newServer(t)
	base := t.TempDir()
	content := payload(100_000, 12)
	s := srv.add("/x.bin", content)
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	srv.handler = func(w http.ResponseWriter, r *http.Request, content []byte) bool {
		w.Header().Set("Content-Length", fmt.Sprint(len(content)))
		w.Write(content[:30_000])
		w.(http.Flusher).Flush()
		close(started)
		<-release
		return true
	}
	ctx, cancel := context.WithCancel(context.Background())
	cfg := testConfig(t, base)
	cfg.Retries = 2
	var got Result
	cfg.OnDone = func(_ metadata.Source, r Result) { got = r }
	go func() {
		<-started
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	summary := New(cfg).DownloadAll(ctx, []metadata.Source{s})
	if summary.Canceled != 1 || got.Status != Canceled || srv.requests.Load() != 1 {
		t.Fatalf("summary %+v, result %+v, requests %d", summary, got, srv.requests.Load())
	}
	assertMissing(t, target(base, s))
	if info, err := os.Stat(target(base, s) + ".part"); err != nil || info.Size() != 30_000 {
		t.Errorf(".part: %v %v", info, err)
	}
}

func TestNotEnoughSpace(t *testing.T) {
	srv := newServer(t)
	base := t.TempDir()
	s := srv.add("/x.bin", payload(10_000, 13))
	cfg := testConfig(t, base)
	cfg.Retries = 2
	cfg.FreeSpace = func(string) (uint64, error) { return 9_999, nil }
	summary, results := run(t, cfg, s)
	if summary.Failed != 1 || srv.requests.Load() != 0 || !strings.Contains(results["x.bin"].Err.Error(), "free space") {
		t.Fatalf("summary %+v, results %+v", summary, results)
	}
}

// TestConcurrencyLimits serves several "hosts" from one listener and
// records how many requests run at once, overall and per host.
func TestConcurrencyLimits(t *testing.T) {
	var (
		mu         sync.Mutex
		active     int
		maxActive  int
		perHost    = make(map[string]int)
		maxPerHost = make(map[string]int)
	)
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, r *http.Request, content []byte) bool {
		mu.Lock()
		active++
		perHost[r.Host]++
		maxActive = max(maxActive, active)
		maxPerHost[r.Host] = max(maxPerHost[r.Host], perHost[r.Host])
		mu.Unlock()
		time.Sleep(60 * time.Millisecond)
		mu.Lock()
		active--
		perHost[r.Host]--
		mu.Unlock()
		return false
	}
	addr := srv.Listener.Addr().String()
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}}

	var sources []metadata.Source
	hosts := []string{"a.example", "b.example", "c.example"}
	for i := 0; i < 12; i++ {
		host := hosts[i%len(hosts)]
		s := srv.add(fmt.Sprintf("/f%d.bin", i), payload(1000, byte(i)))
		s.URI = "http://" + host + "/f" + fmt.Sprint(i) + ".bin"
		sources = append(sources, s)
	}

	cfg := testConfig(t, t.TempDir())
	cfg.Client = client
	cfg.Jobs, cfg.PerHost = 4, 2
	summary, _ := run(t, cfg, sources...)
	if summary.Downloaded != 12 {
		t.Fatalf("summary %+v", summary)
	}
	if maxActive != 4 {
		t.Errorf("max concurrent downloads = %d, want 4", maxActive)
	}
	for _, host := range hosts {
		if maxPerHost[host] > 2 {
			t.Errorf("max concurrent downloads from %s = %d, want <= 2", host, maxPerHost[host])
		}
	}

	// A single host is held to PerHost even with free global slots.
	maxActive = 0
	clear(maxPerHost)
	cfg = testConfig(t, t.TempDir())
	cfg.Client = client
	cfg.Jobs, cfg.PerHost = 8, 2
	for i := range sources {
		sources[i].URI = fmt.Sprintf("http://a.example/f%d.bin", i)
	}
	summary, _ = run(t, cfg, sources...)
	if summary.Downloaded != 12 || maxActive != 2 || maxPerHost["a.example"] != 2 {
		t.Errorf("single host: summary %+v, max %d, per host %v", summary, maxActive, maxPerHost)
	}
}
