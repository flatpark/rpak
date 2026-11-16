package doctor

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/flatpark/rpak/internal/metadata"
	"github.com/flatpark/rpak/internal/prefetch"
	"github.com/flatpark/rpak/internal/summary"
	"github.com/flatpark/rpak/internal/units"
)

// What the extra-data of an app is checked against is in the remote's
// summary, which flatpak keeps a cache of in the repository; flatpak's own
// commands that read it (remote-info, remote-ls) update that cache. So
// doctor fetches the summary itself, the way flatpak does (see package
// summary), into memory only.

// MaxSummary bounds what is downloaded or decompressed of a summary;
// Flathub's whole summary is about 13 MB, its x86_64 subsummary less.
const MaxSummary = 64 << 20

// maxSummary is MaxSummary, smaller in tests.
var maxSummary int64 = MaxSummary

func errTooBig() error { return fmt.Errorf("larger than %s", units.Size(maxSummary)) }

// flatpakArch is flatpak's name for the architecture rpak was built for,
// which is the machine's: rpak is released for amd64 and arm64.
func flatpakArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	case "386":
		return "i386"
	}
	return runtime.GOARCH
}

// summaryKey identifies a fetched summary: subsummaries are per
// architecture and subset.
type summaryKey struct{ url, arch, subset string }

type summaryResult struct {
	once sync.Once
	data []byte
	err  error
}

// summaries memoizes the summaries fetched during a run, and the URL
// checks, which several apps and commits share.
type summaries struct {
	mu     sync.Mutex
	byKey  map[summaryKey]*summaryResult
	checks map[string]*urlResult
}

type urlResult struct {
	once  sync.Once
	check URLCheck
}

// summary is the (sub)summary of a remote for arch, fetched once per run.
func (d *doctor) summary(remoteURL, arch, subset string) ([]byte, error) {
	key := summaryKey{remoteURL, arch, subset}
	d.cache.mu.Lock()
	if d.cache.byKey == nil {
		d.cache.byKey = map[summaryKey]*summaryResult{}
	}
	r := d.cache.byKey[key]
	if r == nil {
		r = &summaryResult{}
		d.cache.byKey[key] = r
	}
	d.cache.mu.Unlock()
	r.once.Do(func() {
		ctx, cancel := withTimeout(d.ctx, d.cfg.SummaryTimeout)
		defer cancel()
		r.data, r.err = FetchSummary(ctx, d.cfg.Client, d.cfg.Version, remoteURL, arch, subset)
	})
	return r.data, r.err
}

// checkURL is CheckURL, once per URL and run.
func (d *doctor) checkURL(rawURL string) URLCheck {
	d.cache.mu.Lock()
	if d.cache.checks == nil {
		d.cache.checks = map[string]*urlResult{}
	}
	r := d.cache.checks[rawURL]
	if r == nil {
		r = &urlResult{}
		d.cache.checks[rawURL] = r
	}
	d.cache.mu.Unlock()
	r.once.Do(func() { r.check = CheckURL(d.ctx, d.cfg.Client, rawURL, d.cfg.Version, d.cfg.NetTimeout) })
	return r.check
}

// errNotFound is a file the remote does not have.
var errNotFound = errors.New("not found")

// FetchSummary gets the summary flatpak would read refs of arch from:
// the subsummary that summary.idx names for arch (and subset), or the
// plain summary of a remote without summary.idx. remoteURL is http(s):
// or file:; nothing is written anywhere.
func FetchSummary(ctx context.Context, client *http.Client, version, remoteURL, arch, subset string) ([]byte, error) {
	get := func(name string) ([]byte, error) { return fetch(ctx, client, version, remoteURL, name) }
	index, err := get("summary.idx")
	switch {
	case errors.Is(err, errNotFound):
		data, err := get("summary")
		if err != nil {
			return nil, fmt.Errorf("summary: %w", err)
		}
		return data, nil
	case err != nil:
		return nil, fmt.Errorf("summary.idx: %w", err)
	}
	subs, err := summary.Subsummaries(index)
	if err != nil {
		return nil, err
	}
	name := summary.SubsummaryName(arch, subset)
	digest, ok := subs[name]
	if !ok {
		return nil, fmt.Errorf("summary.idx has no subsummary %s", name)
	}
	file := "summaries/" + digest + ".gz"
	compressed, err := get(file)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("%s: decompressing: %w", file, err)
	}
	data, err := readLimited(zr)
	if err != nil {
		return nil, fmt.Errorf("%s: decompressing: %w", file, err)
	}
	if err := summary.Verify(data, digest); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return data, nil
}

func readLimited(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxSummary+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxSummary {
		return nil, errTooBig()
	}
	return data, nil
}

// fetch reads a file of a remote into memory.
func fetch(ctx context.Context, client *http.Client, version, remoteURL, name string) ([]byte, error) {
	if path, ok := strings.CutPrefix(remoteURL, "file://"); ok {
		u, err := url.PathUnescape(path)
		if err != nil {
			return nil, err
		}
		f, err := os.Open(filepath.Join(u, name))
		if errors.Is(err, os.ErrNotExist) {
			return nil, errNotFound
		}
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return readLimited(f)
	}
	if !strings.HasPrefix(remoteURL, "http://") && !strings.HasPrefix(remoteURL, "https://") {
		return nil, fmt.Errorf("remotes at %s are not supported", remoteURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(remoteURL, "/")+"/"+name, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", prefetch.UserAgent(version))
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New(errorClass(err))
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, errNotFound
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	case resp.ContentLength > maxSummary:
		return nil, errTooBig()
	}
	data, err := readLimited(resp.Body)
	if err != nil && ctx.Err() != nil {
		return nil, errors.New("timeout")
	}
	return data, err
}

// remoteSubset is the subset of a remote's refs the installation is
// limited to (xa.subset in repo/config), "" for none.
func remoteSubset(inst, remote string) string {
	text, err := os.ReadFile(filepath.Join(inst, "repo", "config"))
	if err != nil {
		return ""
	}
	for _, g := range metadata.KeyFile(string(text)) {
		if g.Name == `remote "`+remote+`"` {
			return g.Values["xa.subset"]
		}
	}
	return ""
}
