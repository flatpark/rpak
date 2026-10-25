package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/flatpark/rpak/internal/metadata"
	"github.com/flatpark/rpak/internal/prefetch"
)

// download runs the downloads, reports progress on stderr and ends with the
// summary line, unless ctx was canceled.
//
// On a terminal a block at the bottom is redrawn in place: a line per
// active download and a total, with bars, speeds and remaining times. Each
// file that is done gets a line above the block, which stays. When the
// downloads end the block is erased, leaving the plain lines, so that
// flatpak's output follows them. Elsewhere, and with TERM=dumb, each event
// gets a time-stamped line, and the progress a line every 10 seconds.
func download(ctx context.Context, cfg prefetch.Config, sources []metadata.Source) prefetch.Summary {
	return newReporter(os.Stderr, detectTerm(os.Stderr), sources).run(ctx, cfg, sources)
}

const (
	redrawInterval    = 250 * time.Millisecond
	heartbeatInterval = 10 * time.Second
	rateWindow        = 4 * time.Second
)

type sample struct {
	at    time.Time
	bytes int64
}

// rate is a transfer rate over the last rateWindow.
type rate struct{ samples []sample }

func (r *rate) add(at time.Time, bytes int64) {
	r.samples = append(r.samples, sample{at, bytes})
	for len(r.samples) > 2 && at.Sub(r.samples[0].at) > rateWindow {
		r.samples = r.samples[1:]
	}
}

// perSecond is the rate, or 0 if it is not known yet.
func (r *rate) perSecond() int64 {
	if len(r.samples) < 2 {
		return 0
	}
	first, last := r.samples[0], r.samples[len(r.samples)-1]
	span := last.at.Sub(first.at)
	if span < 500*time.Millisecond {
		return 0
	}
	return int64(float64(last.bytes-first.bytes) / span.Seconds())
}

// active is a download in progress, keyed by checksum in reporter.active.
type active struct {
	src   metadata.Source
	name  string // sanitized
	start time.Time
	bytes int64
	rate  rate
}

type reporter struct {
	w     io.Writer
	term  term
	now   func() time.Time
	files int
	total int64
	// The widest file name and "done / size" (see sizePair), for columns
	// that do not change width as downloads come and go.
	nameWidth, sizesWidth int

	// In interactive mode (interactive.go) the terminal belongs to flatpak:
	// the reporter is quiet until show, then leaves nothing on screen, no
	// lines for the files that are done and no summary, so that flatpak,
	// which redraws its table relative to the cursor, finds the cursor
	// where it was.
	transient bool

	mu          sync.Mutex
	quiet       bool
	ended       bool          // finish was called
	elapsed     time.Duration // set by finish
	finished    int
	settled     int64 // sizes of the files that are done, whatever the outcome
	transferred int64
	rate        rate
	active      map[string]*active
	order       []string // checksums of active downloads, in start order
	drawn       []int    // widths of the block's lines on screen
}

func newReporter(w io.Writer, t term, sources []metadata.Source) *reporter {
	r := &reporter{w: w, term: t, now: time.Now, files: len(sources), active: make(map[string]*active)}
	for _, s := range sources {
		r.total += s.Size
		r.nameWidth = max(r.nameWidth, textWidth(sanitize(s.Name)))
		r.sizesWidth = max(r.sizesWidth, textWidth(sizePair(s.Size, s.Size)))
	}
	r.sizesWidth = max(r.sizesWidth, textWidth(sizePair(r.total, r.total)))
	return r
}

// run downloads sources with cfg, reporting on the way.
func (r *reporter) run(ctx context.Context, cfg prefetch.Config, sources []metadata.Source) prefetch.Summary {
	cfg.OnStart = r.started
	cfg.OnProgress = r.progress
	cfg.OnDone = r.done
	start := r.now()
	r.mu.Lock()
	r.rate.add(start, 0)
	r.mu.Unlock()

	interval := heartbeatInterval
	if r.term.redraw {
		interval = redrawInterval
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				r.tick()
			case <-stop:
				return
			}
		}
	}()
	summary := prefetch.New(cfg).DownloadAll(ctx, sources)
	close(stop)
	wg.Wait()
	r.finish(summary, ctx.Err() != nil, r.now().Sub(start))
	return summary
}

func (r *reporter) started(s metadata.Source) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	a := &active{src: s, name: sanitize(s.Name), start: now}
	a.rate.add(now, 0)
	r.active[s.Checksum] = a
	r.order = append(r.order, s.Checksum)
	if !r.term.redraw && !r.quiet {
		r.printf("rpak: %s start %s (%s)\n", r.stamp(), a.name, host(s.URI))
	}
}

func (r *reporter) progress(s metadata.Source, n int64) {
	r.mu.Lock()
	r.transferred += n
	if a := r.active[s.Checksum]; a != nil {
		a.bytes += n
	}
	r.mu.Unlock()
}

func (r *reporter) done(s metadata.Source, res prefetch.Result) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.active[s.Checksum]
	if a != nil {
		delete(r.active, s.Checksum)
		for i, c := range r.order {
			if c == s.Checksum {
				r.order = append(r.order[:i], r.order[i+1:]...)
				break
			}
		}
	}
	if res.Status == prefetch.Canceled {
		return
	}
	r.finished++
	r.settled += s.Size
	if r.quiet {
		return
	}
	if !r.term.redraw {
		r.printf("rpak: %s %s\n", r.stamp(), r.event(s, res, a))
		return
	}
	if r.transient {
		r.drawLocked(nil, false)
		return
	}
	// The line goes where the block was, and the block below it at once.
	l := r.resultLine(s, res, a)
	r.drawLocked([]line{l}, false)
}

// event is the text of a line-mode done line. Logs and tests look for
// "done NAME", "reusing NAME", "skipping NAME" and "flatpak will download".
func (r *reporter) event(s metadata.Source, res prefetch.Result, a *active) string {
	name := sanitize(s.Name)
	switch res.Status {
	case prefetch.Downloaded:
		return "done " + name + ", " + r.took(s, a)
	case prefetch.Reused:
		return fmt.Sprintf("reusing %s from an earlier run", name)
	case prefetch.Skipped:
		return fmt.Sprintf("skipping %s: %s", name, errText(res.Err))
	default:
		return fmt.Sprintf("warning: %s: %s; flatpak will download it", name, errText(res.Err))
	}
}

// took is "210.0 MB in 31.2s, 6.7 MB/s" for a downloaded file; a second
// name for the same content (a == nil) was not downloaded on its own.
func (r *reporter) took(s metadata.Source, a *active) string {
	if a == nil {
		return humanSize(s.Size)
	}
	d := r.now().Sub(a.start)
	text := humanSize(s.Size) + " in " + elapsed(d)
	if d > 0 {
		text += ", " + humanRate(int64(float64(a.bytes)/d.Seconds()))
	}
	return text
}

func errText(err error) string {
	if err == nil {
		return "unknown error"
	}
	return sanitize(err.Error())
}

// resultLine is the line a finished file leaves above the block.
func (r *reporter) resultLine(s metadata.Source, res prefetch.Result, a *active) line {
	mark, markSGR, detail, detailSGR := "✓", sgrGreen, "", sgrFaint
	switch res.Status {
	case prefetch.Downloaded:
		detail = r.took(s, a)
	case prefetch.Reused:
		detail = humanSize(s.Size) + ", reused from an earlier run"
	case prefetch.Skipped:
		mark, markSGR, detailSGR = "-", sgrYellow, ""
		detail = "skipped: " + errText(res.Err)
	default:
		mark, markSGR, detailSGR = "✗", sgrRed, ""
		detail = errText(res.Err) + "; flatpak will download it"
	}
	// The name column of the block, so that the details line up with the
	// bars below.
	cols, _ := r.term.size()
	width := r.layout(cols).name
	l := line{{"  ", ""}, {mark, markSGR}, {" ", ""}}
	l.add(padRight(truncateMiddle(sanitize(s.Name), width), width), "")
	l.add("  ", "")
	l.add(detail, detailSGR)
	return l
}

// tick redraws the block, or prints a progress line in line mode.
func (r *reporter) tick() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.quiet {
		r.sampleLocked(r.now())
		return
	}
	if r.term.redraw {
		r.drawLocked(nil, false)
		return
	}
	r.progressLineLocked()
}

// progressLineLocked prints the line-mode progress line.
func (r *reporter) progressLineLocked() {
	now := r.now()
	r.sampleLocked(now)
	if len(r.order) == 0 {
		return
	}
	done := r.doneLocked()
	r.printf("rpak: %s progress: %d/%d files, %s (%d%%), %s, %d active\n",
		r.stamp(), r.finished, r.files, sizePair(done, r.total), percent(done, r.total), humanRate(r.rate.perSecond()), len(r.order))
}

// finish ends the output: the block is erased, and the summary follows
// unless the downloads were interrupted or the reporter is transient.
func (r *reporter) finish(s prefetch.Summary, canceled bool, took time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ended, r.elapsed = true, took
	if r.quiet {
		return
	}
	var lines []line
	if !canceled && !r.transient {
		lines = []line{summaryLine(s, r.transferred, took), nil}
	}
	if r.term.redraw {
		r.drawLocked(lines, true)
		return
	}
	writeLines(r.w, r.term, lines)
}

// show ends the quiet: from now on the progress is shown, at once. It
// reports false, and changes nothing, if the downloads have ended.
func (r *reporter) show() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ended {
		return false
	}
	if r.quiet {
		r.quiet = false
		if r.term.redraw {
			r.drawLocked(nil, false)
		} else {
			r.progressLineLocked()
		}
	}
	return true
}

// summary is the summary line for s, once the downloads have ended.
func (r *reporter) summary(s prefetch.Summary) line {
	r.mu.Lock()
	defer r.mu.Unlock()
	return summaryLine(s, r.transferred, r.elapsed)
}

func (r *reporter) printf(format string, a ...any) { fmt.Fprintf(r.w, format, a...) }

func (r *reporter) stamp() string { return r.now().Format("15:04:05.000") }

func (r *reporter) sampleLocked(now time.Time) {
	r.rate.add(now, r.transferred)
	for _, a := range r.active {
		a.rate.add(now, a.bytes)
	}
}

// doneLocked is how much of the total is behind us: the files that are done
// and what the active downloads have so far.
func (r *reporter) doneLocked() int64 {
	done := r.settled
	for _, a := range r.active {
		done += min(a.bytes, a.src.Size)
	}
	return min(done, r.total)
}

func percent(done, total int64) int {
	if total <= 0 {
		return 100
	}
	return int(done * 100 / total)
}

// drawLocked replaces the block on screen by the lines above, which stay,
// and, unless final, the block as of now. The cursor is kept at the start
// of the line below the block, where typed characters such as "^C" end up
// and get erased with it.
func (r *reporter) drawLocked(above []line, final bool) {
	now := r.now()
	cols, rows := r.term.size()
	var b strings.Builder
	if len(r.drawn) > 0 {
		// Lines that were cut to an earlier, wider terminal take more than
		// one row now if the terminal rewrapped them on resizing.
		up := 0
		for _, w := range r.drawn {
			up += max(1, (w+cols-1)/cols)
		}
		fmt.Fprintf(&b, "\x1b[%dA\r\x1b[J", up)
	}
	width := r.term.width()
	for _, l := range above {
		// Kept whole: these lines are not redrawn, so they may wrap.
		b.WriteString(l.render(r.term.color))
		b.WriteByte('\n')
	}
	r.drawn = r.drawn[:0]
	if !final {
		r.sampleLocked(now)
		for _, l := range r.blockLocked(cols, rows) {
			l = l.clip(width)
			r.drawn = append(r.drawn, l.width())
			b.WriteString(l.render(r.term.color))
			b.WriteByte('\n')
		}
	}
	io.WriteString(r.w, b.String())
}

// progressRow is a line of the block before layout.
type progressRow struct {
	label string
	frac  float64
	pct   string
	sizes string
	speed string
	eta   string
	total bool
}

func newProgressRow(label string, done, size, speed int64) progressRow {
	done = min(done, size)
	row := progressRow{label: label, frac: 1, sizes: sizePair(done, size)}
	if size > 0 {
		row.frac = float64(done) / float64(size)
	}
	row.pct = fmt.Sprintf("%d%%", percent(done, size))
	if speed > 0 {
		row.speed = humanRate(speed)
		row.eta = clock(time.Duration(float64(size-done) / float64(speed) * float64(time.Second)))
	}
	return row
}

// The block's columns: name, bar, percentage, sizes, speed and remaining
// time. Their widths depend only on the files and the terminal width, so
// that they stay put from frame to frame.
const (
	blockIndent  = 4
	blockGap     = 2
	nameMinWidth = 12
	barMinWidth  = 10
	barMaxWidth  = 40
	pctWidth     = 4  // "100%"
	speedWidth   = 10 // "999.9 kB/s"
	etaWidth     = 5  // "01:38", see clock
)

// blockLayout is how the block's columns fit a terminal.
type blockLayout struct {
	name, bar         int // widths; bar 0: no bar
	sizes, speed, eta bool
}

// layout fits the columns in cols. On a narrow terminal the sizes go
// first, then the speed, then the remaining time, then the bar; the name
// is shortened in the middle. The room left goes to the name and the bar
// in turns, the name not taking more than 2/5 of the line, the bar up to
// barMaxWidth.
func (r *reporter) layout(cols int) blockLayout {
	natural := max(r.nameWidth, textWidth(r.totalLabel(r.files)))
	l := blockLayout{name: min(natural, nameMinWidth), bar: barMinWidth, sizes: true, speed: true, eta: true}
	need := func() int {
		n := blockIndent + l.name + blockGap + pctWidth
		if l.bar > 0 {
			n += blockGap + l.bar
		}
		if l.sizes {
			n += blockGap + r.sizesWidth
		}
		if l.speed {
			n += blockGap + speedWidth
		}
		if l.eta {
			n += blockGap + etaWidth
		}
		return n
	}
	avail := max(cols-1, 1)
	for _, drop := range []func(){func() { l.sizes = false }, func() { l.speed = false }, func() { l.eta = false }, func() { l.bar = 0 }} {
		if need() <= avail {
			break
		}
		drop()
	}
	extra := avail - need()
	if extra < 0 {
		l.name = max(l.name+extra, 1)
	}
	growName := func(to int) {
		n := max(min(extra, min(to, natural)-l.name), 0)
		l.name += n
		extra -= n
	}
	growBar := func(to int) {
		if l.bar > 0 {
			n := max(min(extra, to-l.bar), 0)
			l.bar += n
			extra -= n
		}
	}
	growName(24)
	growBar(20)
	growName(avail * 2 / 5)
	growBar(barMaxWidth)
	return l
}

func (r *reporter) totalLabel(finished int) string {
	return fmt.Sprintf("Total %d/%d", finished, r.files)
}

// blockLocked draws the block for a terminal of cols × rows (rows 0:
// unknown). It never takes all rows: the cursor needs the one below it.
// When there are more downloads than fit, the first ones are shown and a
// line says how many more there are.
func (r *reporter) blockLocked(cols, rows int) []line {
	shown, more := len(r.order), 0
	if rows > 0 && shown+2 > rows {
		shown = max(rows-3, 0)
		more = len(r.order) - shown
		if rows < 3 {
			more = 0
		}
	}
	var progress []progressRow
	for _, c := range r.order[:shown] {
		a := r.active[c]
		progress = append(progress, newProgressRow(a.name, a.bytes, a.src.Size, a.rate.perSecond()))
	}
	total := newProgressRow(r.totalLabel(r.finished), r.doneLocked(), r.total, r.rate.perSecond())
	total.total = true
	progress = append(progress, total)

	lay := r.layout(cols)
	indent, gap := strings.Repeat(" ", blockIndent), strings.Repeat(" ", blockGap)
	var lines []line
	for _, p := range progress {
		if p.total && more > 0 {
			lines = append(lines, line{{indent, ""}, {fmt.Sprintf("… and %d more", more), sgrFaint}})
		}
		l := line{{indent, ""}}
		labelSGR := ""
		if p.total {
			labelSGR = sgrBold
		}
		l.add(padRight(truncateMiddle(p.label, lay.name), lay.name), labelSGR)
		if lay.bar > 0 {
			l.add(gap+bar(p.frac, lay.bar), "")
		}
		l.add(gap+padLeft(p.pct, pctWidth), "")
		if lay.sizes {
			l.add(gap+padLeft(p.sizes, r.sizesWidth), sgrFaint)
		}
		if lay.speed {
			l.add(gap+padLeft(p.speed, speedWidth), "")
		}
		if lay.eta {
			l.add(gap+padLeft(p.eta, etaWidth), sgrFaint)
		}
		lines = append(lines, l)
	}
	return lines
}
