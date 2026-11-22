package prefetch

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"

	"github.com/flatpark/rpak/internal/metadata"
)

// Probe asks the servers what the files of sources are now, with HEAD
// requests (no download, and no hit on a download counter that counts
// GETs), Jobs at a time and at most PerHost per host, each within
// ProbeTimeout. It returns the sources the servers say have changed: the
// size they announce is not the source's, or they give a SHA-256 that is
// not. Anything less clear counts as unchanged and is left to the
// download, which checks the file anyway: a failed request, a status
// other than 200, no size, an HTML page (a download link that leads to a
// page) or an encoded body.
func (d *Downloader) Probe(ctx context.Context, sources []metadata.Source) []Change {
	global := make(chan struct{}, d.cfg.Jobs)
	perHost := make(map[string]chan struct{})
	hostOf := func(s metadata.Source) string {
		if u, err := url.Parse(s.URI); err == nil {
			return strings.ToLower(u.Host)
		}
		return ""
	}
	for _, s := range sources {
		if h := hostOf(s); perHost[h] == nil {
			perHost[h] = make(chan struct{}, d.cfg.PerHost)
		}
	}
	changes := make([]*Change, len(sources))
	var wg sync.WaitGroup
	for i, s := range sources {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := acquire(ctx, perHost[hostOf(s)], global)
			if err != nil {
				return
			}
			defer release()
			size, checksum, ok := d.head(ctx, s.URI)
			if !ok {
				return
			}
			if (size >= 0 && size != s.Size) || (checksum != "" && checksum != s.Checksum) {
				got := s
				got.Size, got.Checksum = size, checksum
				if size < 0 {
					got.Size = 0
				}
				changes[i] = &Change{Source: s, Got: got}
			}
		}()
	}
	wg.Wait()
	var out []Change
	for _, c := range changes {
		if c != nil {
			out = append(out, *c)
		}
	}
	return out
}

// head returns the size (-1 if not given) and SHA-256 ("" if not given)
// the server announces for uri; ok is false if it tells nothing usable.
func (d *Downloader) head(ctx context.Context, uri string) (size int64, checksum string, ok bool) {
	ctx, cancel := context.WithTimeout(ctx, d.cfg.ProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, uri, nil)
	if err != nil {
		return 0, "", false
	}
	req.Header.Set("User-Agent", UserAgent(d.cfg.Version))
	// Ask for the digest of RFC 9530; most servers ignore it.
	req.Header.Set("Want-Repr-Digest", "sha-256=10")
	resp, err := d.cfg.Client.Do(req)
	if err != nil {
		return 0, "", false
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, "", false
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "identity") {
		return 0, "", false
	}
	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err == nil && (mt == "text/html" || mt == "application/xhtml+xml") {
		return 0, "", false
	}
	size = resp.ContentLength
	if size == 0 {
		size = -1 // some servers say 0 for HEAD
	}
	checksum = headerSHA256(resp.Header)
	return size, checksum, size >= 0 || checksum != ""
}

// headerSHA256 finds a SHA-256 of the whole file in h, as lowercase hex:
// Repr-Digest (RFC 9530), Digest (RFC 3230), X-Checksum-Sha256
// (Artifactory, Nexus) or x-amz-checksum-sha256 (S3, unless it is the
// checksum of the parts, "...-N").
func headerSHA256(h http.Header) string {
	fromBase64 := func(v string) string {
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v))
		if err != nil || len(b) != 32 {
			return ""
		}
		return hex.EncodeToString(b)
	}
	fromHex := func(v string) string {
		v = strings.ToLower(strings.TrimSpace(v))
		if b, err := hex.DecodeString(v); err != nil || len(b) != 32 {
			return ""
		}
		return v
	}
	// sha-256=:BASE64:, in a comma-separated list.
	for _, v := range h.Values("Repr-Digest") {
		for _, item := range strings.Split(v, ",") {
			alg, val, ok := strings.Cut(strings.TrimSpace(item), "=")
			if ok && strings.EqualFold(alg, "sha-256") {
				if sum := fromBase64(strings.Trim(val, ":")); sum != "" {
					return sum
				}
			}
		}
	}
	// SHA-256=BASE64, in a comma-separated list.
	for _, v := range h.Values("Digest") {
		for _, item := range strings.Split(v, ",") {
			alg, val, ok := strings.Cut(strings.TrimSpace(item), "=")
			if ok && strings.EqualFold(alg, "sha-256") {
				if sum := fromBase64(val); sum != "" {
					return sum
				}
			}
		}
	}
	if sum := fromHex(h.Get("X-Checksum-Sha256")); sum != "" {
		return sum
	}
	if v := h.Get("X-Amz-Checksum-Sha256"); v != "" && !strings.Contains(v, "-") {
		return fromBase64(v)
	}
	return ""
}

// HasFile reports whether dir holds name as a verified copy of s's size
// and checksum.
func HasFile(dir string, s metadata.Source) bool {
	return verifyFile(filepath.Join(dir, s.Name), s.Size, s.Checksum) == nil
}
