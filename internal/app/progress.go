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

// download runs the downloads and reports progress on stderr: a single
// status line redrawn in place on a terminal, one line per event otherwise.
// Failures always get a line of their own.
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
	begun    map[string]time.Time
}

func (r *reporter) started(s metadata.Source) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.begun == nil {
		r.begun = make(map[string]time.Time)
	}
	r.begun[s.Checksum] = time.Now()
	if !r.tty {
		fmt.Fprintf(r.w, "rpak: %s start %s (%s)\n", stamp(), s.Name, host(s.URI))
	}
}

func (r *reporter) progress(n int64) {
	r.mu.Lock()
	r.bytes += n
	r.mu.Unlock()
}

func (r *reporter) done(s metadata.Source, res prefetch.Result) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finished++
	var line string
	switch res.Status {
	case prefetch.Downloaded:
		took := time.Since(r.begun[s.Checksum])
		line = fmt.Sprintf("done %s, %s in %s", s.Name, prefetch.FormatBytes(s.Size), took.Round(100*time.Millisecond))
	case prefetch.Reused:
		r.bytes += s.Size
		line = fmt.Sprintf("reusing %s from an earlier run", s.Name)
	case prefetch.Skipped:
		line = fmt.Sprintf("skipping %s: %v", s.Name, res.Err)
	case prefetch.Failed:
		line = fmt.Sprintf("warning: %s: %v; flatpak will download it", s.Name, res.Err)
	case prefetch.Canceled:
		return
	}
	switch {
	case !r.tty:
		fmt.Fprintf(r.w, "rpak: %s %s\n", stamp(), line)
	case res.Status != prefetch.Downloaded && res.Status != prefetch.Reused:
		// Clear the status line, print the message, the ticker redraws.
		fmt.Fprintf(r.w, "\r\x1b[Krpak: %s\n", line)
	}
}

// redraw rewrites the status line: files done, bytes, current speed.
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
	fmt.Fprintf(r.w, "\r\x1b[Krpak: %d/%d files, %s / %s, %s/s",
		r.finished, r.files, prefetch.FormatBytes(min(r.bytes, r.total)), prefetch.FormatBytes(r.total), prefetch.FormatBytes(speed))
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
