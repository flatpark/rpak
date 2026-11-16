package doctor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/flatpark/rpak/internal/prefetch"
)

// URLCheck is the outcome of checking that a URL can be downloaded.
type URLCheck struct {
	Status  int   // the final HTTP status, 0 if there was none
	Length  int64 // the size of the file, -1 if the server did not say
	Ranged  bool  // HEAD did not do, a one-byte range GET answered
	Elapsed time.Duration
	Err     error // no HTTP answer; see errorClass
}

// OK reports whether the URL answered with a 2xx status.
func (c URLCheck) OK() bool { return c.Err == nil && c.Status >= 200 && c.Status < 300 }

// Outcome is the answer in a few words: "HTTP 200", "HTTP 404", or what
// kind of error there was instead (see errorClass).
func (c URLCheck) Outcome() string {
	if c.Err != nil {
		return errorClass(c.Err)
	}
	return "HTTP " + strconv.Itoa(c.Status)
}

// Timed is Outcome with how long it took: "HTTP 200 in 0.24s", "timeout
// after 10.00s".
func (c URLCheck) Timed() string {
	if c.Err != nil {
		return c.Outcome() + " after " + seconds(c.Elapsed)
	}
	return c.Outcome() + " in " + seconds(c.Elapsed)
}

func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}

// CheckURL asks for a URL without downloading it: HEAD, following
// redirects, and if that gets no 2xx answer (servers that refuse HEAD, or
// signed URLs only valid for GET), a GET of its first byte. timeout covers
// both.
func CheckURL(ctx context.Context, client *http.Client, rawURL, version string, timeout time.Duration) URLCheck {
	ctx, cancel := withTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	check := func(method string) (URLCheck, error) {
		req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
		if err != nil {
			return URLCheck{}, err
		}
		req.Header.Set("User-Agent", prefetch.UserAgent(version))
		if method == http.MethodGet {
			req.Header.Set("Range", "bytes=0-0")
		}
		resp, err := client.Do(req)
		if err != nil {
			return URLCheck{}, err
		}
		// The body is never read: closing it early only costs the
		// connection.
		resp.Body.Close()
		c := URLCheck{Status: resp.StatusCode, Length: resp.ContentLength}
		if resp.StatusCode == http.StatusPartialContent {
			c.Ranged, c.Length = true, rangeTotal(resp.Header.Get("Content-Range"))
		} else if method == http.MethodGet {
			c.Ranged = true
		}
		return c, nil
	}
	c, err := check(http.MethodHead)
	if err == nil && !c.OK() {
		c, err = check(http.MethodGet)
	}
	if err != nil {
		c = URLCheck{Err: err, Length: -1}
	}
	c.Elapsed = time.Since(start)
	return c
}

// rangeTotal is the complete length of "bytes 0-0/12345", -1 if unknown.
func rangeTotal(v string) int64 {
	_, total, ok := strings.Cut(v, "/")
	if !ok {
		return -1
	}
	n, err := strconv.ParseInt(strings.TrimSpace(total), 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// errorClass names what kind of network error err is: DNS, TLS, timeout,
// proxy or connection.
func errorClass(err error) string {
	var dns *net.DNSError
	var certErr *tls.CertificateVerificationError
	var unknownCA x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var invalid x509.CertificateInvalidError
	var record tls.RecordHeaderError
	var netErr net.Error
	var opErr *net.OpError
	msg := err.Error()
	switch {
	case errors.As(err, &dns):
		return "DNS: " + dns.Err + " (" + dns.Name + ")"
	case errors.As(err, &certErr), errors.As(err, &unknownCA), errors.As(err, &hostErr), errors.As(err, &invalid):
		return "TLS: certificate not accepted"
	case errors.As(err, &record), strings.Contains(msg, "tls: "):
		return "TLS: handshake failed"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	case strings.Contains(msg, "proxyconnect"):
		return "proxy: cannot connect to the proxy"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection reset"
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH):
		return "connection: network unreachable"
	case errors.As(err, &opErr):
		return "connection: " + opErr.Err.Error()
	}
	return "error: " + msg
}

type probeResult struct {
	url   string
	label string // where it comes from: "flathub (User installation)"
	check URLCheck
	// skipped says why the URL is not checked.
	skipped string
}

// summaryURL is the URL of a remote's summary file.
func summaryURL(remote string) string {
	return strings.TrimSuffix(remote, "/") + "/summary"
}

// network checks every remote's summary and the fixed URLs, all at once.
func (d *doctor) network(insts []*instInfo) []probeResult {
	var out []*probeResult
	seen := map[string]*probeResult{}
	for _, in := range insts {
		for _, rem := range in.remotes {
			label := rem.Name + " (" + in.inst.Title() + ")"
			switch {
			case strings.HasPrefix(rem.URL, "oci+"), strings.HasPrefix(rem.URL, "file:"):
				out = append(out, &probeResult{url: rem.URL, label: label, skipped: "not over HTTP"})
				continue
			case rem.Has("disabled"):
				out = append(out, &probeResult{url: rem.URL, label: label, skipped: "disabled"})
				continue
			}
			u := summaryURL(rem.URL)
			if p := seen[u]; p != nil {
				p.label += ", " + label
				continue
			}
			p := &probeResult{url: u, label: label}
			seen[u] = p
			out = append(out, p)
		}
	}
	for _, u := range d.cfg.Probes {
		if seen[u] == nil {
			p := &probeResult{url: u}
			seen[u] = p
			out = append(out, p)
		}
	}
	var fns []func()
	for _, p := range out {
		if p.skipped == "" {
			fns = append(fns, func() { p.check = CheckURL(d.ctx, d.cfg.Client, p.url, d.cfg.Version, d.cfg.NetTimeout) })
		}
	}
	parallel(fns...)
	results := make([]probeResult, len(out))
	for i, p := range out {
		results[i] = *p
	}
	return results
}

func (d *doctor) networkSection(results []probeResult) Section {
	s := d.section("Network")
	s.add("proxy for https: %s, for http: %s", d.proxyFor("https://dl.flatpark.org/"), d.proxyFor("http://dl.flatpark.org/"))
	for _, p := range results {
		label := ""
		if p.label != "" {
			label = "  " + p.label
		}
		if p.skipped != "" {
			s.add("%s  not checked: %s%s", RedactURL(p.url), p.skipped, label)
		} else {
			s.add("%s  %s%s", RedactURL(p.url), p.check.Timed(), label)
		}
	}
	return s.done()
}

func seconds(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', 2, 64) + "s"
}
