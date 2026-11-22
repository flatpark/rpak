// Package release finds rpak's releases and installs one in place of the
// running binary (rpak self-update). It reads GitHub's release pages, not
// its API, which limits anonymous requests: the latest release is where
// BASE/latest redirects to, and a release's files are at
// BASE/download/TAG/NAME, with their checksums in SHA256SUMS.
package release

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/flatpark/rpak/internal/prefetch"
)

// DefaultBase is where rpak's releases are.
const DefaultBase = "https://github.com/flatpark/rpak/releases"

// BaseEnv names the environment variable that replaces DefaultBase, for
// tests and mirrors.
const BaseEnv = "RPAK_RELEASE_BASE"

// Base is $RPAK_RELEASE_BASE, or DefaultBase.
func Base(lookupEnv func(string) (string, bool)) string {
	if v, _ := lookupEnv(BaseEnv); v != "" {
		return strings.TrimRight(v, "/")
	}
	return DefaultBase
}

// Asset is the name of the release binary for a Go architecture (amd64,
// arm64), as the release workflow uploads it.
func Asset(goarch string) string { return "rpak-linux-" + goarch }

// SumsName is the file of a release with the sha256 of its binaries.
const SumsName = "SHA256SUMS"

// maxBinary bounds a download: rpak is a few MB.
const maxBinary = 256 << 20

// Client asks for releases.
type Client struct {
	Base string // see Base
	HTTP *http.Client
	// Version is rpak's own, for the User-Agent.
	Version string
}

// NewClient is the client of the running process, honouring the proxy
// settings and $RPAK_RELEASE_BASE.
func NewClient(version string) *Client {
	return &Client{Base: Base(os.LookupEnv), HTTP: prefetch.NewHTTPClient(), Version: version}
}

// URL is the address of a file of the release tag.
func (c *Client) URL(tag, name string) string {
	return c.Base + "/download/" + tag + "/" + name
}

func (c *Client) client() *http.Client {
	if c.HTTP == nil {
		return http.DefaultClient
	}
	return c.HTTP
}

func (c *Client) request(ctx context.Context, method, url string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", prefetch.UserAgent(c.Version))
	return req, nil
}

// Latest is the tag of the latest release: the last part of the path
// BASE/latest redirects to (.../releases/tag/v0.3.6), which is not
// followed. Errors of the request itself are *url.Error, as http.Client
// returns them.
func (c *Client) Latest(ctx context.Context) (string, error) {
	req, err := c.request(ctx, http.MethodHead, c.Base+"/latest")
	if err != nil {
		return "", err
	}
	client := *c.client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
	default:
		return "", fmt.Errorf("HTTP %d from %s, not a redirect to the release", resp.StatusCode, req.URL)
	}
	loc, err := resp.Location()
	if err != nil {
		return "", fmt.Errorf("%s redirects nowhere", req.URL)
	}
	dir, tag := path.Split(strings.TrimRight(loc.Path, "/"))
	if path.Base(dir) != "tag" {
		return "", fmt.Errorf("%s redirects to %s, not to a release", req.URL, loc.Redacted())
	}
	if _, ok := ParseTag(tag); !ok {
		return "", fmt.Errorf("the latest release is %q, not a version vMAJOR.MINOR.PATCH", tag)
	}
	return tag, nil
}

// get asks for url, and returns the body of a 200 answer.
func (c *Client) get(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := c.request(ctx, http.MethodGet, url)
	if err != nil {
		return nil, err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("%s: not found (HTTP 404)", url)
		}
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	return resp.Body, nil
}

// Checksum is the sha256 of the file name of the release tag, from its
// SHA256SUMS ("sha256sum" format).
func (c *Client) Checksum(ctx context.Context, tag, name string) (string, error) {
	url := c.URL(tag, SumsName)
	body, err := c.get(ctx, url)
	if err != nil {
		return "", err
	}
	defer body.Close()
	sc := bufio.NewScanner(io.LimitReader(body, 1<<20))
	for sc.Scan() {
		sum, file, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		file = strings.TrimPrefix(strings.TrimLeft(file, " "), "*") // "*" marks binary mode
		if ok && file == name && len(sum) == sha256.Size*2 && isHex(strings.ToLower(sum)) {
			return strings.ToLower(sum), nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("%s: %w", url, err)
	}
	return "", fmt.Errorf("%s has no checksum of %s", url, name)
}

// Writable checks that a file can be created in dir, by creating one and
// removing it.
func Writable(dir string) error {
	f, err := os.CreateTemp(dir, ".rpak-update-*")
	if err != nil {
		return err
	}
	f.Close()
	return os.Remove(f.Name())
}

// ErrChecksum is the error of a download that does not match SHA256SUMS.
var ErrChecksum = errors.New("checksum mismatch")

// Install downloads the file name of the release tag and puts it in place
// of target, which must be the real path, not a symlink. It downloads into
// a temporary file next to target (.rpak-update-*) and checks it against
// SHA256SUMS; then makes it executable, writes it to disk and renames it
// over target, so that target is either the old binary or the new one
// whatever happens. The temporary file is removed on any failure.
func (c *Client) Install(ctx context.Context, tag, name, target string) (err error) {
	want, err := c.Checksum(ctx, tag, name)
	if err != nil {
		return err
	}
	dir := filepath.Dir(target)
	f, err := os.CreateTemp(dir, ".rpak-update-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
		}
	}()
	url := c.URL(tag, name)
	body, err := c.get(ctx, url)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(body, maxBinary+1))
	body.Close()
	switch {
	case err != nil:
		return fmt.Errorf("downloading %s: %w", url, err)
	case n > maxBinary:
		return fmt.Errorf("%s: larger than %s", url, prefetch.FormatBytes(maxBinary))
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("%w: %s has sha256 %s, %s says %s", ErrChecksum, url, got, SumsName, want)
	}
	if err = f.Chmod(0o755); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, target); err != nil {
		return err
	}
	// The new binary is in place; a failure to sync the directory leaves
	// it there, and is only reported.
	if d, err := os.Open(dir); err == nil {
		defer d.Close()
		if err := d.Sync(); err != nil {
			return fmt.Errorf("installed, but syncing %s: %w", dir, err)
		}
	}
	return nil
}

// IsNetwork reports whether err is a failed request (no HTTP answer), as
// opposed to an answer that is not what was expected.
func IsNetwork(err error) bool {
	var u *url.Error
	return errors.As(err, &u)
}
