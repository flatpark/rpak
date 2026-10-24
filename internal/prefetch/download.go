package prefetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/flatpark/rpak/internal/metadata"
)

// Config tunes a Downloader. NewConfig fills in the defaults.
type Config struct {
	BaseDir string // flatpak user base dir, see UserBaseDir
	Version string // goes into the marker file and the User-Agent

	Jobs    int // concurrent downloads in total
	PerHost int // concurrent downloads per host

	Client      *http.Client
	IdleTimeout time.Duration // fail when no data arrives for this long
	Retries     int           // attempts after the first one
	Backoff     time.Duration // before the first retry, doubled after that

	// FreeSpace reports the bytes available to us on dir's filesystem.
	FreeSpace func(dir string) (uint64, error)

	// Callbacks, called concurrently from download goroutines.
	OnStart    func(metadata.Source)
	OnProgress func(bytes int64)
	OnDone     func(metadata.Source, Result)
}

// NewConfig returns the default configuration: 4 downloads at a time, at
// most 2 per host, 30s to connect and to get response headers, 60s without
// data counts as a failure, 2 retries with exponential backoff.
func NewConfig(baseDir, version string) Config {
	return Config{
		BaseDir:     baseDir,
		Version:     version,
		Jobs:        4,
		PerHost:     2,
		Client:      NewHTTPClient(),
		IdleTimeout: 60 * time.Second,
		Retries:     2,
		Backoff:     time.Second,
		FreeSpace:   freeSpace,
	}
}

// NewHTTPClient returns a client that honours HTTP(S)_PROXY and NO_PROXY.
func NewHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ForceAttemptHTTP2:     true,
		// We want the bytes the checksum was taken of, as flatpak gets them.
		DisableCompression: true,
	}}
}

// Status is what happened to one source.
type Status int

const (
	Downloaded Status = iota // fetched and verified now
	Reused                   // verified file from an earlier rpak run
	Skipped                  // left to flatpak, not an error (e.g. a file of someone else's is there)
	Failed                   // left to flatpak because something went wrong
	Canceled                 // interrupted; the .part file is kept
)

// Result is the outcome for one source.
type Result struct {
	Status Status
	Err    error // why, for Skipped, Failed and Canceled
	Dir    string
}

// Summary is the outcome of DownloadAll.
type Summary struct {
	Downloaded, Reused, Skipped, Failed, Canceled int
	// Dirs are the marked directories that now hold verified files; they
	// are to be removed once flatpak has used them.
	Dirs []string
}

// Downloader fetches extra-data sources into the flatpak user base dir.
type Downloader struct {
	cfg Config
}

// New returns a Downloader; zero fields of cfg other than Retries and the
// callbacks take their NewConfig defaults.
func New(cfg Config) *Downloader {
	def := NewConfig(cfg.BaseDir, cfg.Version)
	if cfg.Jobs <= 0 {
		cfg.Jobs = def.Jobs
	}
	if cfg.PerHost <= 0 {
		cfg.PerHost = def.PerHost
	}
	if cfg.Client == nil {
		cfg.Client = def.Client
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = def.IdleTimeout
	}
	if cfg.Backoff <= 0 {
		cfg.Backoff = def.Backoff
	}
	if cfg.FreeSpace == nil {
		cfg.FreeSpace = def.FreeSpace
	}
	return &Downloader{cfg: cfg}
}

// DownloadAll fetches all sources, Jobs at a time and at most PerHost per
// host, and waits for them. Sources sharing a checksum share a directory
// and are fetched once. Canceling ctx stops all downloads.
func (d *Downloader) DownloadAll(ctx context.Context, sources []metadata.Source) Summary {
	type job struct {
		sources []metadata.Source // same checksum; sources[0] is downloaded
		host    string
	}
	var jobs []*job
	byChecksum := make(map[string]*job)
	for _, s := range sources {
		if j := byChecksum[s.Checksum]; j != nil {
			j.sources = append(j.sources, s)
			continue
		}
		host := ""
		if u, err := url.Parse(s.URI); err == nil {
			host = strings.ToLower(u.Host)
		}
		j := &job{sources: []metadata.Source{s}, host: host}
		byChecksum[s.Checksum] = j
		jobs = append(jobs, j)
	}

	global := make(chan struct{}, d.cfg.Jobs)
	perHost := make(map[string]chan struct{})
	for _, j := range jobs {
		if perHost[j.host] == nil {
			perHost[j.host] = make(chan struct{}, d.cfg.PerHost)
		}
	}

	var (
		mu      sync.Mutex
		summary Summary
		wg      sync.WaitGroup
	)
	record := func(s metadata.Source, r Result) {
		mu.Lock()
		switch r.Status {
		case Downloaded:
			summary.Downloaded++
		case Reused:
			summary.Reused++
		case Skipped:
			summary.Skipped++
		case Failed:
			summary.Failed++
		case Canceled:
			summary.Canceled++
		}
		mu.Unlock()
		if d.cfg.OnDone != nil {
			d.cfg.OnDone(s, r)
		}
	}
	for _, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Take the host slot first so that a job waiting for a busy host
			// does not hold one of the global slots.
			release, err := acquire(ctx, perHost[j.host], global)
			if err != nil {
				for _, s := range j.sources {
					record(s, Result{Status: Canceled, Err: err})
				}
				return
			}
			defer release()
			results, dir := d.fetch(ctx, j.sources)
			if dir != "" {
				mu.Lock()
				summary.Dirs = append(summary.Dirs, dir)
				mu.Unlock()
			}
			for i, s := range j.sources {
				record(s, results[i])
			}
		}()
	}
	wg.Wait()
	return summary
}

func acquire(ctx context.Context, sems ...chan struct{}) (func(), error) {
	var held []chan struct{}
	release := func() {
		for _, sem := range held {
			<-sem
		}
	}
	for _, sem := range sems {
		select {
		case sem <- struct{}{}:
			held = append(held, sem)
		case <-ctx.Done():
			release()
			return nil, ctx.Err()
		}
	}
	return release, nil
}

var errForeign = errors.New("already present, not placed by rpak; leaving it alone")

// fetch handles the sources of one checksum. It returns a result per source
// and the marked directory if it now holds verified files.
func (d *Downloader) fetch(ctx context.Context, sources []metadata.Source) ([]Result, string) {
	first := sources[0]
	dir := ExtraDataDir(d.cfg.BaseDir, first.Checksum)
	results := make([]Result, len(sources))
	fail := func(status Status, err error) ([]Result, string) {
		for i := range results {
			results[i] = Result{Status: status, Err: err, Dir: dir}
		}
		return results, ""
	}

	marker := fmt.Sprintf("rpak %s %s\n", d.cfg.Version, time.Now().UTC().Format(time.RFC3339))
	owned, err := claimDir(dir, marker)
	if err != nil {
		return fail(Failed, err)
	}
	if !owned {
		// Somebody else's directory: never write into it. If the files are
		// there flatpak uses them, otherwise it downloads them itself.
		return fail(Skipped, errForeign)
	}

	target := filepath.Join(dir, first.Name)
	status := Reused
	if err := verifyFile(target, first.Size, first.Checksum); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			// Left over by an earlier run of ours but not right (anymore).
			if err := os.Remove(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fail(Failed, err)
			}
		}
		if d.cfg.OnStart != nil {
			d.cfg.OnStart(first)
		}
		if err := d.download(ctx, first, target); err != nil {
			if ctx.Err() != nil {
				return fail(Canceled, err)
			}
			return fail(Failed, err)
		}
		status = Downloaded
	}
	results[0] = Result{Status: status, Dir: dir}

	// Other names for the same content get a hard link to the verified file.
	for i, s := range sources[1:] {
		alias := filepath.Join(dir, s.Name)
		err := os.Remove(alias)
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			err = os.Link(target, alias)
		}
		if err != nil {
			results[i+1] = Result{Status: Failed, Err: err, Dir: dir}
		} else {
			results[i+1] = Result{Status: status, Dir: dir}
		}
	}
	if err := syncDir(dir); err != nil {
		// Do not leave files flatpak may trust if we could not make them durable.
		os.RemoveAll(dir)
		return fail(Failed, err)
	}
	return results, dir
}

// download fetches s to target via target+".part", retrying as configured.
// Only a file that passed the size and checksum checks is renamed to target.
func (d *Downloader) download(ctx context.Context, s metadata.Source, target string) error {
	part := target + ".part"
	var err error
	for attempt := 0; attempt <= d.cfg.Retries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(d.cfg.Backoff << (attempt - 1)):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		err = d.attempt(ctx, s, part)
		if err == nil {
			if err = os.Rename(part, target); err == nil {
				return nil
			}
		}
		if ctx.Err() != nil {
			// Keep the .part file so the next run can resume.
			return ctx.Err()
		}
		var fatal *fatalError
		if errors.As(err, &fatal) {
			return fatal.err
		}
	}
	return err
}

// fatalError is a failure that retrying does not fix.
type fatalError struct{ err error }

func (e *fatalError) Error() string { return e.err.Error() }

// attempt makes one request, resuming part if it exists, and leaves a
// verified, synced part file on success. A part file with wrong content is
// removed; one that is merely incomplete is kept for the next attempt.
func (d *Downloader) attempt(ctx context.Context, s metadata.Source, part string) error {
	f, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	offset, err := io.Copy(h, f) // hash what we already have
	if err != nil {
		return err
	}
	if offset > s.Size {
		offset = 0 // truncated below
		h.Reset()
	}
	if offset == s.Size {
		return d.finish(f, h, s, part)
	}

	free, err := d.cfg.FreeSpace(filepath.Dir(part))
	if err != nil {
		return fmt.Errorf("checking free space: %w", err)
	}
	if need := uint64(s.Size - offset); free < need {
		return &fatalError{fmt.Errorf("not enough free space: need %s, have %s", FormatBytes(int64(need)), FormatBytes(int64(free)))}
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URI, nil)
	if err != nil {
		return &fatalError{err}
	}
	req.Header.Set("User-Agent", "rpak/"+d.cfg.Version)
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := d.cfg.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusPartialContent && offset > 0:
		if start, ok := contentRangeStart(resp.Header.Get("Content-Range")); !ok || start != offset {
			// Not the range we asked for; start over next time.
			os.Remove(part)
			return fmt.Errorf("bad Content-Range %q for offset %d", resp.Header.Get("Content-Range"), offset)
		}
	case resp.StatusCode == http.StatusOK:
		// The server ignored the Range header: start from scratch.
		offset = 0
		h.Reset()
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && offset > 0:
		os.Remove(part)
		return fmt.Errorf("server rejected resuming at %d", offset)
	default:
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	if err := f.Truncate(offset); err != nil {
		return err
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return err
	}

	// No data for IdleTimeout cancels the request.
	idle := time.AfterFunc(d.cfg.IdleTimeout, func() {
		cancel(fmt.Errorf("no data received for %s", d.cfg.IdleTimeout))
	})
	defer idle.Stop()
	w := io.MultiWriter(f, h)
	buf := make([]byte, 256*1024)
	// Read at most one byte more than expected to notice oversized bodies.
	body := io.LimitReader(resp.Body, s.Size-offset+1)
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			idle.Reset(d.cfg.IdleTimeout)
			if _, err := w.Write(buf[:n]); err != nil {
				return err
			}
			offset += int64(n)
			if d.cfg.OnProgress != nil {
				d.cfg.OnProgress(int64(n))
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			if cause := context.Cause(ctx); cause != nil && ctx.Err() != nil && !errors.Is(cause, context.Canceled) {
				return cause
			}
			return rerr
		}
	}
	return d.finish(f, h, s, part)
}

// finish checks a complete-looking part file and syncs it to disk.
func (d *Downloader) finish(f *os.File, h hash.Hash, s metadata.Source, part string) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() != s.Size {
		if info.Size() > s.Size {
			os.Remove(part)
		}
		return fmt.Errorf("size mismatch: got %d bytes, want %d", info.Size(), s.Size)
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != s.Checksum {
		os.Remove(part)
		return fmt.Errorf("checksum mismatch: got %s", sum)
	}
	return f.Sync()
}

// contentRangeStart parses the first byte position of "bytes START-END/TOTAL".
func contentRangeStart(v string) (int64, bool) {
	rest, ok := strings.CutPrefix(v, "bytes ")
	if !ok {
		return 0, false
	}
	start, _, ok := strings.Cut(rest, "-")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(start), 10, 64)
	return n, err == nil
}

// verifyFile checks that path is a regular file of the given size and
// SHA-256. It returns an fs.ErrNotExist error if there is no such file.
func verifyFile(path string, size int64, checksum string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() != size {
		return fmt.Errorf("size mismatch: got %d bytes, want %d", info.Size(), size)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != checksum {
		return fmt.Errorf("checksum mismatch: got %s", sum)
	}
	return nil
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func freeSpace(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// FormatBytes formats a byte count with binary prefixes, as "1.5 MiB".
func FormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 5; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
