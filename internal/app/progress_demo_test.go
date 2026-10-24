package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/flatpark/rpak/internal/metadata"
	"github.com/flatpark/rpak/internal/prefetch"
)

// TestProgressDemo shows the plan, the progress and the summary of a run of
// throttled downloads, to look at in a terminal; it takes about 20 seconds:
//
//	RPAK_PROGRESS_DEMO=1 go test ./internal/app -run TestProgressDemo -v -count=1
//
// go test pipes the output of tests, so this writes to /dev/tty if there is
// one. RPAK_PROGRESS_DEMO=lines writes to stderr instead, which shows the
// output for logs.
//
// With RPAK_PROGRESS_DEMO_SCREEN=FILE it also writes what the screen shows
// 8 seconds in, without colours, to FILE.
func TestProgressDemo(t *testing.T) {
	if mode := os.Getenv("RPAK_PROGRESS_DEMO"); mode != "1" && mode != "lines" {
		t.Skip("set RPAK_PROGRESS_DEMO=1 to run the demo")
	}
	type file struct {
		id    string
		name  string
		size  int64
		rate  float64 // bytes per second
		reuse bool
		fail  bool
		host  string
	}
	files := []file{
		{id: "com.google.Chrome", name: "google-chrome-stable_131.0.6778.85-1_amd64.deb", size: 28_000_000, rate: 1_900_000, host: "dl.google.com"},
		{id: "com.vendor.enterprise.CollaborationSuite.Desktop", name: "Vendor-Enterprise-Collaboration-Suite-Desktop-Client-2026.10.24-build.4711-linux-x86_64.AppImage", size: 26_000_000, rate: 2_600_000, host: "downloads.vendor-enterprise-collaboration.example"},
		{id: "com.sogou.PinyinPro", name: "微信输入法-专业版-安装包.deb", size: 9_000_000, rate: 700_000, host: "pinyin.example.cn"},
		{id: "org.example.EmojiFonts", name: "fonts-✨-emoji-pack.tar.gz", size: 5_500_000, rate: 900_000, host: "github.com"},
		{id: "org.example.Broken", name: "broken-mirror.bin", size: 4_000_000, fail: true, host: "mirror.example.org"},
		{id: "org.freedesktop.Platform.openh264//2.4.1", name: "openh264-2.4.1-linux64.7.so.bz2", size: 650_000, reuse: true, host: "ciscobinary.openh264.org"},
		{id: "com.example.Licensed", name: "LICENSE.txt", size: 300_000, rate: 60_000, host: "example.com"},
		{id: "net.example.SDK.Docs", name: "sdk-docs.zip", size: 12_000_000, rate: 3_000_000, host: "docs.example.net"},
	}

	contents := make(map[string][]byte)
	byName := make(map[string]file)
	var sources []metadata.Source
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		name := strings.TrimPrefix(req.URL.Path, "/")
		f, data := byName[name], contents[name]
		if f.fail {
			time.Sleep(1500 * time.Millisecond)
			http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		start := time.Now()
		sent := 0
		for sent < len(data) {
			// A rate that drifts by ±40% over a few seconds.
			el := time.Since(start).Seconds()
			r := f.rate * (1 + 0.4*math.Sin(el*1.3+float64(len(name))))
			if f.rate == 0 {
				r = 1e9
			}
			n := min(32*1024, len(data)-sent)
			if _, err := w.Write(data[sent : sent+n]); err != nil {
				return
			}
			sent += n
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			select {
			case <-time.After(time.Duration(float64(n) / r * float64(time.Second))):
			case <-req.Context().Done():
				return
			}
		}
	}))
	defer srv.Close()

	rng := rand.New(rand.NewSource(1))
	for _, f := range files {
		data := make([]byte, f.size)
		rng.Read(data)
		sum := sha256.Sum256(data)
		contents[f.name], byName[f.name] = data, f
		sources = append(sources, metadata.Source{
			Ref:      demoRef(f.id),
			Name:     f.name,
			URI:      "http://" + f.host + "/" + url.PathEscape(f.name),
			Size:     f.size,
			Checksum: hex.EncodeToString(sum[:]),
		})
	}

	cfg := prefetch.NewConfig(t.TempDir(), "demo")
	cfg.Retries = 1
	cfg.Backoff = 500 * time.Millisecond
	// Every host is the test server.
	addr := srv.Listener.Addr().String()
	cfg.Client = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}}
	// One file is there from an "earlier run".
	for i, f := range files {
		if f.reuse {
			if s := prefetch.New(cfg).DownloadAll(context.Background(), sources[i:i+1]); s.Downloaded != 1 {
				t.Fatalf("setting up the reused file: %+v", s)
			}
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// What download does, on the terminal: go test pipes stderr.
	dest := os.Stderr
	if os.Getenv("RPAK_PROGRESS_DEMO") != "lines" && !isTerminal(dest) {
		if tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0); err == nil {
			defer tty.Close()
			dest = tty
		}
	}
	tm := detectTerm(dest)
	var out io.Writer = dest
	if path := os.Getenv("RPAK_PROGRESS_DEMO_SCREEN"); path != "" {
		scr := &screen{}
		out = io.MultiWriter(dest, scr)
		timer := time.AfterFunc(8*time.Second, func() { _ = os.WriteFile(path, []byte(scr.text()+"\n"), 0o644) })
		defer timer.Stop()
	}
	writePlan(out, tm, sources, "")
	summary := newReporter(out, tm, sources).run(ctx, cfg, sources)
	if ctx.Err() != nil {
		fmt.Fprintln(dest, "rpak: interrupted")
		return
	}
	if summary.Downloaded != 6 || summary.Reused != 1 || summary.Failed != 1 {
		t.Errorf("summary %+v", summary)
	}
}

// demoRef makes a ref of "ID" or "ID//BRANCH".
func demoRef(id string) string {
	id, branch, ok := strings.Cut(id, "//")
	if !ok {
		return "app/" + id + "/x86_64/stable"
	}
	return "runtime/" + id + "/x86_64/" + branch
}
