package app

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/flatpark/rpak/internal/metadata"
	"github.com/flatpark/rpak/internal/prefetch"
)

func TestHumanSize(t *testing.T) {
	for n, want := range map[int64]string{
		0: "0 bytes", 1: "1 byte", 999: "999 bytes", 1000: "1.0 kB", 650_000: "650.0 kB",
		999_949: "999.9 kB", 999_950: "1.0 MB", 412_300_000: "412.3 MB", 2_400_000_000: "2.4 GB",
	} {
		if got := humanSize(n); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", n, got, want)
		}
	}
	for _, c := range []struct {
		done, total int64
		want        string
	}{
		{88_100_000, 210_000_000, "88.1 / 210.0 MB"},
		{880_000, 210_000_000, "0.9 / 210.0 MB"},
		{0, 210_000_000, "0.0 / 210.0 MB"},
		{999_960, 999_960, "1.0 / 1.0 MB"},
		{300, 900, "300 / 900 bytes"},
	} {
		if got := sizePair(c.done, c.total); got != c.want {
			t.Errorf("sizePair(%d, %d) = %q, want %q", c.done, c.total, got, c.want)
		}
	}
	if got := humanRate(6_200_000); got != "6.2 MB/s" {
		t.Errorf("humanRate: %q", got)
	}
	if got := humanRate(512); got != "512 B/s" {
		t.Errorf("humanRate: %q", got)
	}
}

func TestClock(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0: "00:00", 98 * time.Second: "01:38", 3599 * time.Second: "59:59", time.Hour + 5*time.Minute: "1h05m",
		12 * time.Hour: "12h", 100 * time.Hour: "--:--", -time.Second: "--:--",
	} {
		if got := clock(d); got != want || textWidth(got) > etaWidth {
			t.Errorf("clock(%s) = %q, want %q", d, got, want)
		}
	}
	for d, want := range map[time.Duration]string{
		4 * time.Second: "4.0s", 31234 * time.Millisecond: "31.2s", 125 * time.Second: "2m05s", 2*time.Hour + 3*time.Minute: "2h03m",
	} {
		if got := elapsed(d); got != want {
			t.Errorf("elapsed(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestBar(t *testing.T) {
	for _, c := range []struct {
		frac  float64
		width int
		want  string
	}{
		{0, 4, "    "},
		{1, 4, "████"},
		{0.5, 4, "██  "},
		{0.55, 4, "██▏ "},
		{0.99, 4, "███▉"},
		{1.5, 2, "██"},
		{-1, 2, "  "},
	} {
		if got := bar(c.frac, c.width); got != c.want || textWidth(got) != c.width {
			t.Errorf("bar(%v, %d) = %q, want %q", c.frac, c.width, got, c.want)
		}
	}
}

func TestAppID(t *testing.T) {
	for ref, want := range map[string]string{
		"app/com.example.App/x86_64/stable":                      "com.example.App",
		"app/com.example.App/x86_64/beta":                        "com.example.App//beta",
		"runtime/org.freedesktop.Platform.openh264/x86_64/2.4.1": "org.freedesktop.Platform.openh264//2.4.1",
		"odd": "odd",
	} {
		if got := appID(ref); got != want {
			t.Errorf("appID(%q) = %q, want %q", ref, got, want)
		}
	}
}

func planSources() []metadata.Source {
	return []metadata.Source{
		{Ref: "app/com.example.App/x86_64/stable", Name: "app.deb", URI: "https://github.com/x/app.deb", Size: 210_000_000, Checksum: "aa"},
		{Ref: "app/com.example.App/x86_64/stable", Name: "data.tar.gz", URI: "https://github.com/x/data.tar.gz", Size: 1_500_000, Checksum: "bb"},
		{Ref: "app/com.sogou.PinyinPro/x86_64/stable", Name: "微信输入法-专业版-安装包.deb", URI: "https://pinyin.example.cn/p.deb", Size: 9_000_000, Checksum: "cc"},
		{Ref: "runtime/org.freedesktop.Platform.openh264/x86_64/2.4.1", Name: "openh264-2.4.1-linux64.7.so.bz2", URI: "http://ciscobinary.openh264.org/o.bz2", Size: 650_000, Checksum: "dd"},
	}
}

// render renders lines as writeLines does for a width.
func render(lines []line, width int, color bool) string {
	var b bytes.Buffer
	writeLines(&b, term{tty: width > 0, color: color, cols: width + 1}, lines)
	return b.String()
}

func TestPlanLines(t *testing.T) {
	sources := planSources()
	// No limit: everything, as it goes to logs.
	got := render(planLines(sources, "", 0), 0, false)
	want := `rpak: prefetching 4 extra-data file(s):

  ID                                        File                                 Size  Host
  com.example.App                           app.deb                          210.0 MB  github.com
  com.example.App                           data.tar.gz                        1.5 MB  github.com
  com.sogou.PinyinPro                       微信输入法-专业版-安装包.deb       9.0 MB  pinyin.example.cn
  org.freedesktop.Platform.openh264//2.4.1  openh264-2.4.1-linux64.7.so.bz2  650.0 kB  ciscobinary.openh264.org
                                            4 files                          221.2 MB

`
	if got != want {
		t.Errorf("no limit:\n%s\nwant:\n%s", got, want)
	}

	// 80 columns: the host goes rather than the ID getting too short.
	got = render(planLines(sources, "", 79), 79, false)
	want = `rpak: prefetching 4 extra-data file(s):

  ID                                  File                                 Size
  com.example.App                     app.deb                          210.0 MB
  com.example.App                     data.tar.gz                        1.5 MB
  com.sogou.PinyinPro                 微信输入法-专业版-安装包.deb       9.0 MB
  org.freedesktop.Platfo…h264//2.4.1  openh264-2.4.1-linux64.7.so.bz2  650.0 kB
                                      4 files                          221.2 MB

`
	if got != want {
		t.Errorf("80 columns:\n%s\nwant:\n%s", got, want)
	}

	// Narrower: the host goes, then the ID; then the file name is cut.
	for _, c := range []struct {
		width    int
		host, id bool
	}{
		{110, true, true}, {100, true, true}, {79, false, true}, {70, false, true},
		{60, false, false}, {50, false, false}, {30, false, false}, {12, false, false},
	} {
		lines := planLines(sources, "", c.width)
		text := render(lines, c.width, false)
		header := lines[2].clip(c.width).render(false)
		if strings.Contains(header, "Host") != c.host || strings.Contains(header, "ID") != c.id {
			t.Errorf("width %d: header %q", c.width, header)
		}
		for _, l := range lines {
			if w := l.clip(c.width).width(); w > c.width {
				t.Errorf("width %d: a line is %d wide:\n%s", c.width, w, text)
			}
		}
		if c.width >= 30 && !strings.Contains(text, "221.2 MB") {
			t.Errorf("width %d: no total:\n%s", c.width, text)
		}
	}
}

func TestPlanLinesColor(t *testing.T) {
	lines := planLines(planSources()[:1], "", 0)
	if got := render(lines, 0, false); strings.Contains(got, "\x1b") {
		t.Errorf("escape sequences without colour: %q", got)
	}
	got := render(lines, 0, true)
	if !strings.Contains(got, "\x1b[1mID") || !strings.Contains(got, "\x1b[2mgithub.com\x1b[0m") {
		t.Errorf("colours: %q", got)
	}
	// A single file has no total line.
	if strings.Contains(got, "1 file") {
		t.Errorf("total for one file: %q", got)
	}
}

func TestPlanDryRun(t *testing.T) {
	var b bytes.Buffer
	printPlan(&b, planSources()[:1], "/home/u/.local/share/flatpak")
	want := `rpak: dry run, would prefetch 1 extra-data file(s):

  ID               File         Size  Host
  com.example.App  app.deb  210.0 MB  github.com
    -> /home/u/.local/share/flatpak/extra-data/aa

`
	if b.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", b.String(), want)
	}
	b.Reset()
	printPlan(&b, nil, "/base")
	if b.String() != "rpak: nothing to prefetch\n" {
		t.Errorf("got %q", b.String())
	}
	b.Reset()
	printPlan(&b, nil, "")
	if b.String() != "" {
		t.Errorf("got %q", b.String())
	}
	// On a terminal, the directory is shortened in the middle.
	lines := planLines(planSources()[:1], "/home/someone/with/a/long/path/.local/share/flatpak", 39)
	dir := lines[4].clip(39).render(false)
	if !strings.HasPrefix(dir, "    -> /home/") || !strings.HasSuffix(dir, "/aa") || textWidth(dir) > 39 {
		t.Errorf("dir line %q", dir)
	}
}

func TestLineClip(t *testing.T) {
	l := line{{"  ", ""}, {"name.deb", sgrBold}, {"   ", ""}, {"faint", sgrFaint}, {"   ", ""}}
	if got := l.clip(0).render(true); got != "  \x1b[1mname.deb\x1b[0m   \x1b[2mfaint\x1b[0m" {
		t.Errorf("trailing spaces: %q", got)
	}
	if got := l.clip(8).render(true); got != "  \x1b[1mname.…\x1b[0m" {
		t.Errorf("clip 8: %q", got)
	}
	if got := l.clip(13).render(false); got != "  name.deb  …" {
		t.Errorf("clip 13: %q", got)
	}
}

func TestSummaryLine(t *testing.T) {
	s := prefetch.Summary{Downloaded: 4, Reused: 1, Failed: 1}
	l := summaryLine(s, 290_000_000, 23400*time.Millisecond)
	if got := l.render(false); got != "rpak: prefetch done: 5 ready (4 downloaded, 1 reused), 1 skipped/failed, 290.0 MB in 23.4s" {
		t.Errorf("got %q", got)
	}
	if got := l.render(true); !strings.Contains(got, "\x1b[32m5 ready\x1b[0m") || !strings.Contains(got, "\x1b[33m1 skipped/failed\x1b[0m") {
		t.Errorf("colours: %q", got)
	}
	l = summaryLine(prefetch.Summary{Downloaded: 2}, 1000, time.Second)
	if got := l.render(true); strings.Contains(got, "\x1b[33m") {
		t.Errorf("yellow without failures: %q", got)
	}
}

// openPTY returns the terminal side of a new pseudo-terminal of cols × rows.
func openPTY(t *testing.T, cols, rows int) *os.File {
	t.Helper()
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no pseudo-terminals: %v", err)
	}
	t.Cleanup(func() { ptmx.Close() })
	var unlock int32
	var n uint32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, ptmx.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); errno != 0 {
		t.Skipf("unlockpt: %v", errno)
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, ptmx.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); errno != 0 {
		t.Skipf("ptsname: %v", errno)
	}
	pts, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("opening the pseudo-terminal: %v", err)
	}
	t.Cleanup(func() { pts.Close() })
	ws := struct{ Row, Col, X, Y uint16 }{uint16(rows), uint16(cols), 0, 0}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, pts.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws))); errno != 0 {
		t.Fatalf("setting the size: %v", errno)
	}
	return pts
}

func TestDetectTerm(t *testing.T) {
	pts := openPTY(t, 77, 11)
	for _, c := range []struct {
		term, noColor string
		redraw, color bool
	}{
		{"xterm-256color", "", true, true},
		{"xterm-256color", "1", true, false},
		{"dumb", "", false, false},
		{"", "", false, false},
	} {
		t.Setenv("TERM", c.term)
		t.Setenv("NO_COLOR", c.noColor)
		got := detectTerm(pts)
		if !got.tty || got.redraw != c.redraw || got.color != c.color {
			t.Errorf("TERM=%q NO_COLOR=%q: %+v", c.term, c.noColor, got)
		}
		if cols, rows := got.size(); cols != 77 || rows != 11 || got.width() != 76 {
			t.Errorf("size %d×%d, width %d", cols, rows, got.width())
		}
	}
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	t.Setenv("TERM", "xterm")
	if got := detectTerm(devNull); got.tty || got.redraw || got.color || got.width() != 0 {
		t.Errorf("/dev/null: %+v", got)
	}
	if got := detectTerm(&bytes.Buffer{}); got.tty {
		t.Errorf("buffer: %+v", got)
	}
}
