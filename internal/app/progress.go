package app

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/flatpark/rpak/internal/metadata"
	"github.com/flatpark/rpak/internal/prefetch"
)

// download runs the downloads and reports progress on stderr. On a terminal
// a block is redrawn in place: one line per active download and a summary
// line, so concurrent downloads are visible. Otherwise one line per event.
// Each finished file also gets a line of its own.
func download(ctx context.Context, cfg prefetch.Config, sources []metadata.Source) prefetch.Summary {
	r := &reporter{w: os.Stderr, tty: isTerminal(os.Stderr), files: len(sources), start: time.Now()}
	for _, s := range sources {
		r.total += s.Size
	}
	cfg.OnStart = r.started
	cfg.OnProgress = r.progress
	cfg.OnDone = r.done

	if !r.tty {
		return prefetch.New(cfg).DownloadAll(ctx, sources)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(250 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				r.redraw()
			case <-stop:
				return
			}
		}
	}()
	summary := prefetch.New(cfg).DownloadAll(ctx, sources)
	close(stop)
	wg.Wait()
	r.redraw()
	fmt.Fprintln(r.w)
	return summary
}

type sample struct {
	at    time.Time
	bytes int64
}

// active is a download in progress, keyed by checksum in reporter.active.
type active struct {
	src   metadata.Source
	start time.Time
	bytes int64
}

type reporter struct {
	w     io.Writer
	tty   bool
	files int
	total int64
	start time.Time

	mu       sync.Mutex
	finished int
	bytes    int64 // transferred, plus the sizes of reused files
	samples  []sample
	active   map[string]*active
	order    []string // checksums of active downloads, in start order
	drawn    int      // lines of the block currently on screen
}

func (r *reporter) started(s metadata.Source) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active == nil {
		r.active = make(map[string]*active)
	}
	r.active[s.Checksum] = &active{src: s, start: time.Now()}
	r.order = append(r.order, s.Checksum)
	if !r.tty {
		fmt.Fprintf(r.w, "rpak: %s start %s (%s)\n", stamp(), s.Name, host(s.URI))
	}
}

func (r *reporter) progress(s metadata.Source, n int64) {
	r.mu.Lock()
	r.bytes += n
	if a := r.active[s.Checksum]; a != nil {
		a.bytes += n
	}
	r.mu.Unlock()
}

func (r *reporter) done(s metadata.Source, res prefetch.Result) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var began time.Time
	if a := r.active[s.Checksum]; a != nil {
		began = a.start
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
	var line string
	switch res.Status {
	case prefetch.Downloaded:
		took := time.Since(began)
		line = fmt.Sprintf("done %s, %s in %s", s.Name, prefetch.FormatBytes(s.Size), took.Round(100*time.Millisecond))
	case prefetch.Reused:
		r.bytes += s.Size
		line = fmt.Sprintf("reusing %s from an earlier run", s.Name)
	case prefetch.Skipped:
		line = fmt.Sprintf("skipping %s: %v", s.Name, res.Err)
	case prefetch.Failed:
		line = fmt.Sprintf("warning: %s: %v; flatpak will download it", s.Name, res.Err)
	}
	if !r.tty {
		fmt.Fprintf(r.w, "rpak: %s %s\n", stamp(), line)
		return
	}
	// Print the message where the block was; the ticker redraws below it.
	r.clearBlock()
	fmt.Fprintf(r.w, "rpak: %s\n", line)
}

// clearBlock moves to the first line of the block on screen and clears it
// and everything below. The cursor stays at the end of the block's last
// line between redraws, hence drawn-1 lines up.
func (r *reporter) clearBlock() {
	if r.drawn > 1 {
		fmt.Fprintf(r.w, "\x1b[%dA", r.drawn-1)
	}
	fmt.Fprint(r.w, "\r\x1b[J")
	r.drawn = 0
}

// redraw rewrites the block: a line per active download, then files done,
// bytes and the current overall speed.
func (r *reporter) redraw() {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	r.samples = append(r.samples, sample{now, r.bytes})
	// Speed over the last ~3 seconds.
	for len(r.samples) > 2 && now.Sub(r.samples[0].at) > 3*time.Second {
		r.samples = r.samples[1:]
	}
	var speed int64
	if first := r.samples[0]; now.Sub(first.at) > 0 {
		speed = int64(float64(r.bytes-first.bytes) / now.Sub(first.at).Seconds())
	}

	lines := make([]string, 0, len(r.order)+1)
	for _, c := range r.order {
		a := r.active[c]
		pct := 0
		if a.src.Size > 0 {
			pct = int(min(a.bytes, a.src.Size) * 100 / a.src.Size)
		}
		lines = append(lines, fmt.Sprintf("  %3d%%  %s / %s  %s",
			pct, prefetch.FormatBytes(min(a.bytes, a.src.Size)), prefetch.FormatBytes(a.src.Size), a.src.Name))
	}
	lines = append(lines, fmt.Sprintf("rpak: %d/%d files, %d active, %s / %s, %s/s",
		r.finished, r.files, len(r.order), prefetch.FormatBytes(min(r.bytes, r.total)), prefetch.FormatBytes(r.total), prefetch.FormatBytes(speed)))

	// Lines must not wrap, or moving up by line count lands in the wrong place.
	width := termWidth(r.w)
	r.clearBlock()
	for i, l := range lines {
		if width > 1 && len([]rune(l)) > width-1 {
			l = string([]rune(l)[:width-1])
		}
		if i > 0 {
			fmt.Fprint(r.w, "\n")
		}
		fmt.Fprint(r.w, l)
	}
	r.drawn = len(lines)
}

func stamp() string { return time.Now().Format("15:04:05.000") }

func host(uri string) string {
	if u, err := url.Parse(uri); err == nil {
		return u.Host
	}
	return "?"
}

// isTerminal reports whether f is a terminal (TCGETS succeeds), unlike a
// character-device check, which /dev/null also passes.
func isTerminal(f *os.File) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&t)))
	return errno == 0
}

// termWidth returns the column count of w's terminal, or 0 if unknown.
func termWidth(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok {
		return 0
	}
	var ws struct{ Row, Col, X, Y uint16 }
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws)))
	if errno != 0 {
		return 0
	}
	return int(ws.Col)
}
