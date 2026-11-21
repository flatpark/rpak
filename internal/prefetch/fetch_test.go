package prefetch

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flatpark/rpak/internal/metadata"
)

// entries lists what is in the extra-data directory, but the directory
// of checksum keep.
func entries(t *testing.T, base string) []string {
	t.Helper()
	es, _ := os.ReadDir(filepath.Join(base, "extra-data"))
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

func TestFetch(t *testing.T) {
	base := t.TempDir()
	srv := newServer(t)
	content := payload(300000, 3)
	srv.add("/new/app_2.0.deb", content)
	cfg := testConfig(t, base)
	var started, done int
	var progress int64
	cfg.OnStart = func(s metadata.Source) {
		started++
		if s.Size != int64(len(content)) || s.Name != "app.deb" || s.Checksum != "" {
			t.Errorf("OnStart %+v", s)
		}
	}
	cfg.OnProgress = func(_ metadata.Source, n int64) { progress += n }
	cfg.OnDone = func(s metadata.Source, r Result) {
		done++
		if r.Status != Downloaded {
			t.Errorf("OnDone %+v", r)
		}
	}
	src, dir, err := New(cfg).Fetch(context.Background(), srv.URL+"/new/app_2.0.deb", "app.deb")
	if err != nil {
		t.Fatal(err)
	}
	if src.Checksum != sum(content) || src.Size != int64(len(content)) || src.URI != srv.URL+"/new/app_2.0.deb" || src.Name != "app.deb" {
		t.Errorf("source %+v", src)
	}
	if dir != ExtraDataDir(base, sum(content)) || !hasMarker(dir) {
		t.Errorf("dir %s", dir)
	}
	// Where flatpak looks, and nothing else: no temporary directory, no
	// part file.
	assertFile(t, filepath.Join(dir, "app.deb"), content)
	if es := entries(t, base); len(es) != 1 || es[0] != sum(content) {
		t.Errorf("extra-data: %v", es)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*.part")); len(left) != 0 {
		t.Errorf("left: %v", left)
	}
	if started != 1 || done != 1 || progress != int64(len(content)) {
		t.Errorf("callbacks: %d %d %d", started, done, progress)
	}
	if ua, _ := srv.ua.Load().(string); ua != "rpak/test" {
		t.Errorf("User-Agent %q", ua)
	}

	// The same file again: the marked directory is reused, a damaged file
	// in it replaced.
	os.WriteFile(filepath.Join(dir, "app.deb"), []byte("damaged"), 0o644)
	cfg.OnStart, cfg.OnProgress, cfg.OnDone = nil, nil, nil
	src2, dir2, err := New(cfg).Fetch(context.Background(), srv.URL+"/new/app_2.0.deb", "app.deb")
	if err != nil || src2 != src || dir2 != dir {
		t.Fatalf("again: %+v %s %v", src2, dir2, err)
	}
	assertFile(t, filepath.Join(dir, "app.deb"), content)
	if es := entries(t, base); len(es) != 1 {
		t.Errorf("extra-data: %v", es)
	}

	// Someone else's directory is left alone, and not handed back.
	os.RemoveAll(dir)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "app.deb"), []byte("mine"), 0o644)
	src3, dir3, err := New(cfg).Fetch(context.Background(), srv.URL+"/new/app_2.0.deb", "app.deb")
	if err != nil || dir3 != "" || src3.Checksum != src.Checksum {
		t.Fatalf("foreign: %+v %q %v", src3, dir3, err)
	}
	assertFile(t, filepath.Join(dir, "app.deb"), []byte("mine"))
	if es := entries(t, base); len(es) != 1 {
		t.Errorf("extra-data: %v", es)
	}
}

// TestFetchFails: a failed download leaves nothing behind; 404 is not
// retried, a short body is.
func TestFetchFails(t *testing.T) {
	base := t.TempDir()
	srv := newServer(t)
	cfg := testConfig(t, base)
	cfg.Retries = 2
	_, dir, err := New(cfg).Fetch(context.Background(), srv.URL+"/missing.deb", "app.deb")
	if err == nil || dir != "" || !strings.Contains(err.Error(), "404") {
		t.Errorf("404: %q %v", dir, err)
	}
	if n := srv.requests.Load(); n != 1 {
		t.Errorf("404 was asked for %d times", n)
	}
	if es := entries(t, base); len(es) != 0 {
		t.Errorf("left after 404: %v", es)
	}

	content := payload(1000, 1)
	srv.add("/short.deb", content)
	srv.handler = func(w http.ResponseWriter, r *http.Request, content []byte) bool {
		w.Header().Set("Content-Length", "1000")
		w.Write(content[:500])
		return true
	}
	srv.requests.Store(0)
	var status Status = -1
	cfg.OnDone = func(_ metadata.Source, r Result) { status = r.Status }
	_, _, err = New(cfg).Fetch(context.Background(), srv.URL+"/short.deb", "app.deb")
	if err == nil || srv.requests.Load() != 3 || status != Failed {
		t.Errorf("short body: %v after %d requests, status %v", err, srv.requests.Load(), status)
	}
	if es := entries(t, base); len(es) != 0 {
		t.Errorf("left after a short body: %v", es)
	}

	srv.add("/empty.deb", nil)
	srv.handler = nil
	if _, _, err := New(cfg).Fetch(context.Background(), srv.URL+"/empty.deb", "app.deb"); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("empty: %v", err)
	}
	if _, _, err := New(cfg).Fetch(context.Background(), srv.URL+"/x", "../x"); err == nil {
		t.Errorf("a name with a slash was taken")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := New(cfg).Fetch(ctx, srv.URL+"/short.deb", "app.deb"); err == nil {
		t.Errorf("canceled: no error")
	}
	if es := entries(t, base); len(es) != 0 {
		t.Errorf("left: %v", es)
	}
}
