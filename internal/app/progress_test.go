package app

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/flatpark/rpak/internal/metadata"
	"github.com/flatpark/rpak/internal/prefetch"
)

// fakeRun drives a reporter by hand, with a clock that only moves when told.
type fakeRun struct {
	t   *testing.T
	r   *reporter
	scr *screen
	now time.Time
}

func newFakeRun(t *testing.T, tm term, sources []metadata.Source) *fakeRun {
	f := &fakeRun{t: t, scr: &screen{}, now: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	f.r = newReporter(f.scr, tm, sources)
	f.r.now = func() time.Time { return f.now }
	f.r.rate.add(f.now, 0) // as run does
	return f
}

// advance moves the clock in redraw steps, with each active download
// receiving bytes at its rate (bytes per second).
func (f *fakeRun) advance(d time.Duration, rates map[string]int64) {
	for step := time.Duration(0); step < d; step += redrawInterval {
		f.now = f.now.Add(redrawInterval)
		for _, s := range f.sources() {
			if rate, ok := rates[s.Name]; ok {
				f.r.progress(s, rate/int64(time.Second/redrawInterval))
			}
		}
		f.r.tick()
	}
}

func (f *fakeRun) sources() []metadata.Source {
	f.r.mu.Lock()
	defer f.r.mu.Unlock()
	var out []metadata.Source
	for _, c := range f.r.order {
		out = append(out, f.r.active[c].src)
	}
	return out
}

func source(name string, size int64) metadata.Source {
	return metadata.Source{Ref: "app/org.example." + strings.Split(name, ".")[0] + "/x86_64/stable", Name: name,
		URI: "https://dl.example.org/" + name, Size: size, Checksum: fmt.Sprintf("%x", sha256.Sum256([]byte(name)))}
}

var sgr = regexp.MustCompile("\x1b\\[[0-9;]*m")

// checkWidths fails if a line of the block on screen is wider than width.
func (f *fakeRun) checkWidths(width int) {
	f.t.Helper()
	for _, l := range strings.Split(f.scr.text(), "\n") {
		if strings.HasPrefix(l, "  ✓") || strings.HasPrefix(l, "  ✗") || strings.HasPrefix(l, "  -") {
			continue // result lines may wrap; they are not redrawn
		}
		if w := textWidth(l); w > width {
			f.t.Errorf("line %d cells wide, more than %d: %q", w, width, l)
		}
	}
}

func testSources() []metadata.Source {
	return []metadata.Source{
		source("app.deb", 210_000_000),
		source("Vendor-Collaboration-Suite-Desktop-2026.10.24-x86_64.AppImage", 120_000_000),
		source("微信输入法-专业版.deb", 9_000_000),
		source("small.txt", 300_000),
	}
}

// startTestRun starts three downloads, fails one, reuses the fourth file.
func startTestRun(f *fakeRun, sources []metadata.Source) {
	f.r.started(sources[0])
	f.r.started(sources[1])
	f.r.started(sources[2])
	f.advance(4*time.Second, map[string]int64{sources[0].Name: 6_000_000, sources[1].Name: 2_000_000, sources[2].Name: 500_000})
	f.r.done(sources[2], prefetch.Result{Status: prefetch.Failed, Err: errors.New("HTTP 503 Service Unavailable")})
	f.r.done(sources[3], prefetch.Result{Status: prefetch.Reused})
	f.advance(2*time.Second, map[string]int64{sources[0].Name: 6_000_000, sources[1].Name: 2_000_000})
}

func TestProgressBlock(t *testing.T) {
	sources := testSources()
	f := newFakeRun(t, term{tty: true, redraw: true, cols: 100, rows: 30}, sources)
	startTestRun(f, sources)
	want := `  ✗ 微信输入法-专业版.deb           HTTP 503 Service Unavailable; flatpak will download it
  ✓ small.txt                       300.0 kB, reused from an earlier run
    app.deb                         ███▍                   17%   36.0 / 210.0 MB    6.0 MB/s  00:29
    Vendor-Collaboration….AppImage  ██                     10%   12.0 / 120.0 MB    2.0 MB/s  00:54
    Total 2/4                       ███▍                   16%   57.3 / 339.3 MB    8.2 MB/s  00:34`
	if got := f.scr.text(); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	f.checkWidths(99)

	// Finishing leaves the result lines and the summary, then a blank line.
	f.advance(time.Second, map[string]int64{sources[0].Name: 6_000_000, sources[1].Name: 2_000_000})
	f.r.done(sources[1], prefetch.Result{Status: prefetch.Downloaded})
	f.r.done(sources[0], prefetch.Result{Status: prefetch.Downloaded})
	f.r.finish(prefetch.Summary{Downloaded: 2, Reused: 1, Failed: 1}, false, 7*time.Second)
	want = `  ✗ 微信输入法-专业版.deb           HTTP 503 Service Unavailable; flatpak will download it
  ✓ small.txt                       300.0 kB, reused from an earlier run
  ✓ Vendor-Collaboration….AppImage  120.0 MB in 7.0s, 2.0 MB/s
  ✓ app.deb                         210.0 MB in 7.0s, 6.0 MB/s
rpak: prefetch done: 3 ready (2 downloaded, 1 reused), 1 skipped/failed, 58.0 MB in 7.0s`
	if got := f.scr.text(); got != want {
		t.Errorf("at the end:\n%s\nwant:\n%s", got, want)
	}
	if raw := f.scr.raw.String(); sgr.MatchString(raw) || !strings.HasSuffix(raw, "\n\n") {
		t.Errorf("colours without colour, or no blank line at the end: %q", raw[max(len(raw)-200, 0):])
	}
}

func TestProgressColor(t *testing.T) {
	sources := testSources()
	f := newFakeRun(t, term{tty: true, redraw: true, color: true, cols: 100, rows: 30}, sources)
	startTestRun(f, sources)
	raw := f.scr.raw.String()
	for _, want := range []string{"\x1b[31m✗\x1b[0m", "\x1b[32m✓\x1b[0m", "\x1b[1mTotal 2/4", "\x1b[2m300.0 kB, reused"} {
		if !strings.Contains(raw, want) {
			t.Errorf("no %q in %q", want, raw)
		}
	}
	// The screen shows the same text as without colours.
	g := newFakeRun(t, term{tty: true, redraw: true, cols: 100, rows: 30}, sources)
	startTestRun(g, sources)
	if f.scr.text() != g.scr.text() {
		t.Errorf("with colours:\n%s\nwithout:\n%s", f.scr.text(), g.scr.text())
	}
}

func TestProgressNarrow(t *testing.T) {
	sources := testSources()
	for _, c := range []struct {
		cols int
		want string // the line of app.deb
	}{
		{100, "    app.deb                         ███▍                   17%   36.0 / 210.0 MB    6.0 MB/s  00:29"},
		{80, "    app.deb               █▋           17%   36.0 / 210.0 MB    6.0 MB/s  00:29"},
		// The sizes go first, then the speed, then the remaining time.
		{70, "    app.deb                   ██▍              17%    6.0 MB/s  00:29"},
		{50, "    app.deb               █▋           17%  00:29"},
		{40, "    app.deb            █▋           17%"},
		// Then the bar.
		{20, "    app.deb     17%"},
		{8, "    … …"},
	} {
		f := newFakeRun(t, term{tty: true, redraw: true, cols: c.cols, rows: 30}, sources)
		startTestRun(f, sources)
		f.checkWidths(c.cols - 1)
		lines := strings.Split(f.scr.text(), "\n")
		if got := lines[2]; got != c.want {
			t.Errorf("%d columns: got\n%q, want\n%q\n%s", c.cols, got, c.want, f.scr.text())
		}
	}
}

func TestProgressManyDownloads(t *testing.T) {
	var sources []metadata.Source
	for i := range 10 {
		sources = append(sources, source(fmt.Sprintf("file%02d.bin", i), 1_000_000))
	}
	f := newFakeRun(t, term{tty: true, redraw: true, cols: 80, rows: 8}, sources)
	for _, s := range sources {
		f.r.started(s)
	}
	f.advance(time.Second, nil)
	lines := strings.Split(f.scr.text(), "\n")
	// 8 rows: 7 for the block, the last one for the cursor.
	if len(lines) != 7 || !strings.HasPrefix(lines[4], "    file04.bin") ||
		lines[5] != "    … and 5 more" || !strings.HasPrefix(lines[6], "    Total 0/10") {
		t.Errorf("got:\n%s", f.scr.text())
	}
	// Tiny terminals still get the total.
	for _, rows := range []int{3, 2, 1} {
		f := newFakeRun(t, term{tty: true, redraw: true, cols: 80, rows: rows}, sources)
		for _, s := range sources {
			f.r.started(s)
		}
		f.advance(time.Second, nil)
		lines := strings.Split(f.scr.text(), "\n")
		if !strings.HasPrefix(lines[len(lines)-1], "    Total 0/10") || len(lines) > max(rows-1, 1) {
			t.Errorf("%d rows:\n%s", rows, f.scr.text())
		}
	}
}

func TestProgressResize(t *testing.T) {
	sources := testSources()
	f := newFakeRun(t, term{tty: true, redraw: true, cols: 100, rows: 30}, sources[:2])
	f.r.started(sources[0])
	f.r.started(sources[1])
	f.advance(time.Second, map[string]int64{sources[0].Name: 1_000_000})
	widths := slicesClone(f.r.drawn)
	if len(widths) != 3 {
		t.Fatalf("block of %d lines", len(widths))
	}
	// The terminal gets narrower: the lines on screen took two rows each
	// after rewrapping, so the redraw goes up by more.
	f.r.term.cols = 50
	up := 0
	for _, w := range widths {
		up += (w + 49) / 50
	}
	before := f.scr.raw.Len()
	f.r.tick()
	if raw := f.scr.raw.String()[before:]; !strings.HasPrefix(raw, fmt.Sprintf("\x1b[%dA\r\x1b[J", up)) || up <= 3 {
		t.Errorf("up %d, redraw %q", up, raw)
	}
	f.checkWidths(49)
}

func slicesClone(s []int) []int { return append([]int(nil), s...) }

func TestProgressCanceled(t *testing.T) {
	sources := testSources()
	f := newFakeRun(t, term{tty: true, redraw: true, cols: 100, rows: 30}, sources)
	f.r.started(sources[0])
	f.advance(time.Second, map[string]int64{sources[0].Name: 1_000_000})
	f.scr.Write([]byte("^C")) // the terminal's echo
	f.r.done(sources[0], prefetch.Result{Status: prefetch.Canceled, Err: errors.New("canceled")})
	f.r.finish(prefetch.Summary{Canceled: 4}, true, time.Second)
	if got := f.scr.text(); got != "" {
		t.Errorf("left on screen: %q", got)
	}
}

func TestProgressLines(t *testing.T) {
	sources := testSources()
	f := newFakeRun(t, term{}, sources)
	f.r.started(sources[0])
	f.r.started(sources[2])
	for range 2 {
		f.now = f.now.Add(heartbeatInterval)
		f.r.progress(sources[0], 50_000_000)
		f.r.tick()
	}
	f.r.done(sources[2], prefetch.Result{Status: prefetch.Failed, Err: errors.New("HTTP 503 Service Unavailable\x1b[2J")})
	f.r.done(sources[3], prefetch.Result{Status: prefetch.Reused})
	f.r.done(sources[1], prefetch.Result{Status: prefetch.Skipped, Err: errors.New("already present, not placed by rpak; leaving it alone")})
	f.now = f.now.Add(5 * time.Second)
	f.r.progress(sources[0], 110_000_000)
	f.r.done(sources[0], prefetch.Result{Status: prefetch.Downloaded})
	f.r.finish(prefetch.Summary{Downloaded: 1, Reused: 1, Skipped: 1, Failed: 1}, false, 25*time.Second)
	want := `rpak: 12:00:00.000 start app.deb (dl.example.org)
rpak: 12:00:00.000 start 微信输入法-专业版.deb (dl.example.org)
rpak: 12:00:10.000 progress: 0/4 files, 50.0 / 339.3 MB (14%), 5.0 MB/s, 2 active
rpak: 12:00:20.000 progress: 0/4 files, 100.0 / 339.3 MB (29%), 5.0 MB/s, 2 active
rpak: 12:00:20.000 warning: 微信输入法-专业版.deb: HTTP 503 Service Unavailable?[2J; flatpak will download it
rpak: 12:00:20.000 reusing small.txt from an earlier run
rpak: 12:00:20.000 skipping Vendor-Collaboration-Suite-Desktop-2026.10.24-x86_64.AppImage: already present, not placed by rpak; leaving it alone
rpak: 12:00:25.000 done app.deb, 210.0 MB in 25.0s, 8.4 MB/s
rpak: prefetch done: 2 ready (1 downloaded, 1 reused), 2 skipped/failed, 210.0 MB in 25.0s

`
	if got := f.scr.raw.String(); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	// What the end-to-end test looks for.
	for _, re := range []string{`(?m)^rpak: [0-9:.]+ start `, `(?m)^rpak: [0-9:.]+ done `} {
		if !regexp.MustCompile(re).MatchString(want) {
			t.Errorf("no match for %s", re)
		}
	}
}
