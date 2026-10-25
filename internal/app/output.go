package app

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/flatpark/rpak/internal/metadata"
	"github.com/flatpark/rpak/internal/prefetch"
)

// Terminal output of install and update: the plan table, the progress
// block (progress.go) and the summary. On a terminal lines are cut to its
// width so that none wraps; colours are used only on a terminal, without
// NO_COLOR, and not with TERM=dumb or unset, which also get no redrawing.
// Anything else gets plain lines meant for logs and grep. rpak clean has
// its own output (clean.go).

// term describes where the output goes.
type term struct {
	tty    bool // a terminal: lines are cut to its width
	redraw bool // cursor movement works: the progress block is redrawn in place
	color  bool
	f      *os.File // for the size; nil for a fixed size
	cols   int      // fixed size, used when f is nil
	rows   int
}

func detectTerm(w io.Writer) term {
	f, ok := w.(*os.File)
	if !ok || !isTerminal(f) {
		return term{}
	}
	t := os.Getenv("TERM")
	smart := t != "" && t != "dumb"
	return term{tty: true, redraw: smart, color: smart && os.Getenv("NO_COLOR") == "", f: f}
}

// size returns the terminal's columns and rows, now: it is asked again for
// every frame, so a resize takes effect at the next redraw. Unknown values
// are 0, except that a terminal of unknown width is taken as 80 wide.
func (t term) size() (cols, rows int) {
	if !t.tty {
		return 0, 0
	}
	cols, rows = t.cols, t.rows
	if t.f != nil {
		cols, rows = termSize(t.f)
	}
	if cols <= 0 {
		cols = 80
	}
	return cols, rows
}

// width is the number of cells a line may take, or 0 for no limit. The
// last column stays free: writing into it leaves some terminals about to
// wrap.
func (t term) width() int {
	cols, _ := t.size()
	if cols == 0 {
		return 0
	}
	return max(cols-1, 1)
}

// SGR codes; restrained on purpose: secondary information is faint, and
// green, yellow and red mean done, left to flatpak and failed.
const (
	sgrBold   = "1"
	sgrFaint  = "2"
	sgrRed    = "31"
	sgrGreen  = "32"
	sgrYellow = "33"
)

// seg is a piece of a line with one style.
type seg struct {
	text string
	sgr  string
}

// line is a line of output built from segments, so that it can be measured
// and cut without the escape sequences getting in the way.
type line []seg

func (l *line) add(text, sgr string) { *l = append(*l, seg{text, sgr}) }

func (l line) width() int {
	n := 0
	for _, s := range l {
		n += textWidth(s.text)
	}
	return n
}

// clip cuts l to width cells (0: no limit), ending it with "…" if it had
// to cut, and drops trailing spaces.
func (l line) clip(width int) line {
	out := make(line, 0, len(l))
	if width <= 0 || l.width() <= width {
		out = append(out, l...)
	} else {
		room := width - 1
		for _, s := range l {
			w := textWidth(s.text)
			if w <= room {
				out = append(out, s)
				room -= w
				continue
			}
			out = append(out, seg{headCells(s.text, room) + ellipsis, s.sgr})
			break
		}
	}
	for len(out) > 0 {
		last := &out[len(out)-1]
		last.text = strings.TrimRight(last.text, " ")
		if last.text != "" {
			break
		}
		out = out[:len(out)-1]
	}
	return out
}

func (l line) render(color bool) string {
	var b strings.Builder
	for _, s := range l {
		if color && s.sgr != "" && s.text != "" {
			b.WriteString("\x1b[" + s.sgr + "m" + s.text + "\x1b[0m")
		} else {
			b.WriteString(s.text)
		}
	}
	return b.String()
}

// writeLines writes lines cut to t's width, in colour if t has it.
func writeLines(w io.Writer, t term, lines []line) {
	var b strings.Builder
	width := t.width()
	for _, l := range lines {
		b.WriteString(l.clip(width).render(t.color))
		b.WriteByte('\n')
	}
	io.WriteString(w, b.String())
}

// humanSize formats a size as flatpak does (g_format_size): decimal
// units, "412.3 MB", for all of rpak's output.
func humanSize(n int64) string {
	if n < 1000 {
		if n == 1 {
			return "1 byte"
		}
		return fmt.Sprintf("%d bytes", n)
	}
	v := float64(n)
	for _, unit := range []string{"kB", "MB", "GB", "TB", "PB"} {
		v /= 1000
		if v < 999.95 {
			return fmt.Sprintf("%.1f %s", v, unit)
		}
	}
	return fmt.Sprintf("%.1f EB", v/1000)
}

// humanRate formats bytes per second, "6.2 MB/s".
func humanRate(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d B/s", max(n, 0))
	}
	return humanSize(n) + "/s"
}

// sizePair formats "done / total" in the unit of total: "88.1 / 210.0 MB",
// "0.0 / 210.0 MB". As done is at most total, so is its width.
func sizePair(done, total int64) string {
	t := humanSize(total)
	_, unit, _ := strings.Cut(t, " ")
	div := 1.0
	for _, u := range []string{"kB", "MB", "GB", "TB", "PB", "EB"} {
		div *= 1000
		if u == unit {
			return fmt.Sprintf("%.1f / %s", float64(done)/div, t)
		}
	}
	return fmt.Sprintf("%d / %s", done, t)
}

// clock formats a remaining time in five cells at most, like flatpak for
// less than an hour: "01:38", then "1h05m", "12h".
func clock(d time.Duration) string {
	s := int64(d.Round(time.Second) / time.Second)
	switch {
	case s < 0 || s >= 100*3600:
		return "--:--"
	case s >= 10*3600:
		return fmt.Sprintf("%dh", s/3600)
	case s >= 3600:
		return fmt.Sprintf("%dh%02dm", s/3600, s/60%60)
	}
	return fmt.Sprintf("%02d:%02d", s/60, s%60)
}

// elapsed formats a duration for the done lines and the summary: "31.2s",
// "2m05s".
func elapsed(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	s := int64(d.Round(time.Second) / time.Second)
	if s < 3600 {
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh%02dm", s/3600, s/60%60)
}

// partialBlocks are the eighths of a cell flatpak's progress bar uses.
var partialBlocks = []string{"", "▏", "▎", "▍", "▌", "▋", "▊", "▉"}

// bar draws a progress bar of width cells, like flatpak's: full blocks,
// one partial block, then spaces.
func bar(frac float64, width int) string {
	frac = min(max(frac, 0), 1)
	eighths := int(frac * float64(width*8))
	full, part := eighths/8, eighths%8
	s := strings.Repeat("█", full)
	if full < width {
		s += partialBlocks[part]
		if part > 0 {
			full++
		}
	}
	return s + strings.Repeat(" ", width-full)
}

func host(uri string) string {
	if u, err := url.Parse(uri); err == nil && u.Host != "" {
		return sanitize(u.Host)
	}
	return "?"
}

// appID is the ID column of the plan: the ID of the ref, with its branch
// unless it is "stable", the branch of nearly all apps ("app/org.x.Y/x86_64/
// stable" is "org.x.Y", "runtime/org.x.Ext/x86_64/2.4" is "org.x.Ext//2.4").
func appID(ref string) string {
	parts := strings.Split(ref, "/")
	if len(parts) != 4 || parts[1] == "" {
		return sanitize(ref)
	}
	if parts[3] == "stable" || parts[3] == "" {
		return sanitize(parts[1])
	}
	return sanitize(parts[1] + "//" + parts[3])
}

// printPlan shows what is going to be prefetched, a row per file, on w.
// baseDir is set for a dry run, which also shows where each file goes and
// says when there is nothing to prefetch.
func printPlan(w io.Writer, sources []metadata.Source, baseDir string) {
	writePlan(w, detectTerm(w), sources, baseDir)
}

func writePlan(w io.Writer, t term, sources []metadata.Source, baseDir string) {
	if len(sources) == 0 {
		if baseDir != "" {
			fmt.Fprintln(w, "rpak: nothing to prefetch")
		}
		return
	}
	writeLines(w, t, planLines(sources, baseDir, t.width()))
}

// planColumn is a column of the plan table.
type planColumn struct {
	title  string
	cells  []string
	right  bool // right-aligned
	min    int  // the width it may be shrunk to; 0: never shrunk
	shrink int  // columns with lower values are shrunk first
	width  int
}

// planLines lays out the plan table for a width (0: no limit). The host
// is shortened first, then the ID and the file name, the longer one first,
// all in the middle. Before the ID gets shorter than 24 cells or the file
// name than 40, the host and then the ID are dropped.
func planLines(sources []metadata.Source, baseDir string, width int) []line {
	var total int64
	id := &planColumn{title: "ID", min: 24, shrink: 2}
	name := &planColumn{title: "File", min: 40, shrink: 2}
	size := &planColumn{title: "Size", right: true}
	hosts := &planColumn{title: "Host", min: 16, shrink: 1}
	for _, s := range sources {
		total += s.Size
		id.cells = append(id.cells, appID(s.Ref))
		name.cells = append(name.cells, sanitize(s.Name))
		size.cells = append(size.cells, humanSize(s.Size))
		hosts.cells = append(hosts.cells, host(s.URI))
	}
	count := fmt.Sprintf("%d files", len(sources))
	if len(sources) == 1 {
		count = "1 file"
	}
	totalSize := humanSize(total)
	for _, c := range []*planColumn{id, name, size, hosts} {
		c.width = textWidth(c.title)
		for _, cell := range c.cells {
			c.width = max(c.width, textWidth(cell))
		}
	}
	name.width = max(name.width, textWidth(count))
	size.width = max(size.width, textWidth(totalSize))

	const indent, gap = 2, 2
	cols := fit(width, indent, gap, [][]*planColumn{
		{id, name, size, hosts},
		{id, name, size},
		{name, size},
	})

	title := fmt.Sprintf("rpak: prefetching %d extra-data file(s):", len(sources))
	if baseDir != "" {
		title = fmt.Sprintf("rpak: dry run, would prefetch %d extra-data file(s):", len(sources))
	}
	lines := []line{{{title, ""}}, nil}
	row := func(cell func(c *planColumn) (string, string)) line {
		l := line{{strings.Repeat(" ", indent), ""}}
		for i, c := range cols {
			text, sgr := cell(c)
			text = truncateMiddle(text, c.width)
			if c.right {
				text = padLeft(text, c.width)
			} else if i < len(cols)-1 {
				text = padRight(text, c.width)
			}
			if i > 0 {
				l.add(strings.Repeat(" ", gap), "")
			}
			l.add(text, sgr)
		}
		return l
	}
	lines = append(lines, row(func(c *planColumn) (string, string) { return c.title, sgrBold }))
	for i, s := range sources {
		lines = append(lines, row(func(c *planColumn) (string, string) {
			if c == hosts {
				return c.cells[i], sgrFaint
			}
			return c.cells[i], ""
		}))
		if baseDir != "" {
			dir := sanitize(prefetch.ExtraDataDir(baseDir, s.Checksum))
			l := line{{strings.Repeat(" ", indent+2) + "-> ", sgrFaint}}
			if width > 0 {
				dir = truncateMiddle(dir, width-l.width())
			}
			l.add(dir, sgrFaint)
			lines = append(lines, l)
		}
	}
	if len(sources) > 1 {
		lines = append(lines, row(func(c *planColumn) (string, string) {
			switch c {
			case name:
				return count, sgrFaint
			case size:
				return totalSize, sgrFaint
			}
			return "", ""
		}))
	}
	return append(lines, nil)
}

// fit picks the first set of columns that fits in width and sets their
// widths. To fit, columns are shrunk down to their minimum width, those
// with the lowest shrink value first and, among equals, the widest a cell
// at a time. In the last set, which is used whatever its width, the
// columns that can be shrunk give up all the room still needed.
func fit(width, indent, gap int, sets [][]*planColumn) []*planColumn {
	for i, set := range sets {
		last := i == len(sets)-1
		need := indent + gap*(len(set)-1)
		widths := make([]int, len(set))
		for j, c := range set {
			need += c.width
			widths[j] = c.width
		}
		if width <= 0 || need <= width {
			return set
		}
		over := need - width
		shrinkable := func(j int, floor int) bool { return set[j].min > 0 && widths[j] > floor }
		pick := func(floors func(int) int) int {
			best := -1
			for j, c := range set {
				if !shrinkable(j, floors(j)) {
					continue
				}
				if b := best; b < 0 || c.shrink < set[b].shrink || c.shrink == set[b].shrink && widths[j] > widths[b] {
					best = j
				}
			}
			return best
		}
		for over > 0 {
			j := pick(func(j int) int { return set[j].min })
			if j < 0 && last {
				j = pick(func(int) int { return 1 })
			}
			if j < 0 {
				break
			}
			widths[j]--
			over--
		}
		if over == 0 || last {
			for j, c := range set {
				c.width = widths[j]
			}
			return set
		}
	}
	return sets[len(sets)-1]
}

// summaryLine is the line left after prefetching, before flatpak's output.
// Its wording is relied on by tests and logs.
func summaryLine(s prefetch.Summary, transferred int64, took time.Duration) line {
	ready, unready := s.Downloaded+s.Reused, s.Skipped+s.Failed
	l := line{{"rpak: prefetch done: ", ""}}
	l.add(fmt.Sprintf("%d ready", ready), sgrGreen)
	l.add(fmt.Sprintf(" (%d downloaded, %d reused), ", s.Downloaded, s.Reused), "")
	unreadySGR := ""
	if unready > 0 {
		unreadySGR = sgrYellow
	}
	l.add(fmt.Sprintf("%d skipped/failed", unready), unreadySGR)
	l.add(fmt.Sprintf(", %s in %s", humanSize(transferred), elapsed(took)), sgrFaint)
	return l
}

// isTerminal reports whether f is a terminal (TCGETS succeeds), unlike a
// character-device check, which /dev/null also passes.
func isTerminal(f *os.File) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&t)))
	return errno == 0
}

// termSize returns the columns and rows of f's terminal, 0 if unknown.
func termSize(f *os.File) (cols, rows int) {
	var ws struct{ Row, Col, X, Y uint16 }
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws)))
	if errno != 0 {
		return 0, 0
	}
	return int(ws.Col), int(ws.Row)
}
