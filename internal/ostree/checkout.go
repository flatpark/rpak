package ostree

import (
	"compress/flate"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ErrNotFound is an object the repository does not serve.
var ErrNotFound = errors.New("not found")

// Source serves the files of a repository.
type Source interface {
	// Open opens a file by its path in the repository; a missing file
	// is ErrNotFound.
	Open(ctx context.Context, path string) (io.ReadCloser, error)
}

// NewSource is the repository at a remote's URL: http:, https: or file:.
func NewSource(rawURL string, client *http.Client, userAgent string) (Source, error) {
	switch {
	case strings.HasPrefix(rawURL, "file://"):
		p, err := url.PathUnescape(strings.TrimPrefix(rawURL, "file://"))
		if err != nil {
			return nil, err
		}
		return dirSource(p), nil
	case strings.HasPrefix(rawURL, "http://") || strings.HasPrefix(rawURL, "https://"):
		return &httpSource{base: strings.TrimSuffix(rawURL, "/"), client: client, userAgent: userAgent}, nil
	}
	return nil, fmt.Errorf("remotes at %s are not supported", rawURL)
}

type dirSource string

func (d dirSource) Open(_ context.Context, path string) (io.ReadCloser, error) {
	f, err := os.Open(filepath.Join(string(d), filepath.FromSlash(path)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return f, err
}

type httpSource struct {
	base      string
	client    *http.Client
	userAgent string
}

func (h *httpSource) Open(ctx context.Context, path string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.base+"/"+path, nil)
	if err != nil {
		return nil, err
	}
	if h.userAgent != "" {
		req.Header.Set("User-Agent", h.userAgent)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return resp.Body, nil
	case http.StatusNotFound:
		resp.Body.Close()
		return nil, ErrNotFound
	}
	resp.Body.Close()
	return nil, fmt.Errorf("%s: HTTP %d", path, resp.StatusCode)
}

// maxMeta bounds a metadata object; ostree's own limit is
// OSTREE_MAX_METADATA_SIZE, 128 MB, real ones are kilobytes.
const maxMeta = 128 << 20

// errNoLooseObjects explains a missing object: the repository only serves
// static deltas, which rpak does not read.
func errNoLooseObjects(path string) error {
	return fmt.Errorf("%s is not served as a loose object (the repository may only offer static deltas, which rpak cannot read)", path)
}

// readMeta fetches a metadata object and checks its checksum.
func readMeta(ctx context.Context, src Source, checksum, kind string) ([]byte, error) {
	path := ObjectPath(checksum, kind)
	r, err := src.Open(ctx, path)
	if errors.Is(err, ErrNotFound) {
		return nil, errNoLooseObjects(path)
	}
	if err != nil {
		return nil, err
	}
	defer r.Close()
	data, err := io.ReadAll(io.LimitReader(r, maxMeta+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(data) > maxMeta {
		return nil, fmt.Errorf("%s: too large", path)
	}
	if err := verifyMeta(data, checksum); err != nil {
		return nil, err
	}
	return data, nil
}

// ReadCommit fetches a commit object and checks it.
func ReadCommit(ctx context.Context, src Source, checksum string) (Commit, error) {
	if !ValidChecksum(checksum) {
		return Commit{}, fmt.Errorf("%q is not a commit checksum", checksum)
	}
	data, err := readMeta(ctx, src, checksum, "commit")
	if err != nil {
		return Commit{}, err
	}
	return ParseCommit(data)
}

// fileJobs bounds the file objects fetched at once.
const fileJobs = 8

// Checkout writes the tree of a commit to dir, which must not exist,
// checking every object against its checksum on the way: the commit
// against the checksum the caller trusts, everything else against the
// checksums in the objects already checked. Ownership and xattrs are not
// restored; modes are, without setuid/setgid/sticky bits, and always
// readable and writable by the owner (flatpak build-export canonicalizes
// permissions anyway). On error dir is left for the caller to remove.
func Checkout(ctx context.Context, src Source, commit Commit, dir string) error {
	type fileJob struct{ path, checksum string }
	type dirMode struct {
		path string
		mode uint32
	}
	var files []fileJob
	var dirs []dirMode
	var walk func(path, tree, meta string, depth int) error
	walk = func(path, tree, meta string, depth int) error {
		if depth > 256 {
			return fmt.Errorf("%s: directories nested too deeply", path)
		}
		metaData, err := readMeta(ctx, src, meta, "dirmeta")
		if err != nil {
			return err
		}
		mode, err := ParseDirMeta(metaData)
		if err != nil {
			return err
		}
		treeData, err := readMeta(ctx, src, tree, "dirtree")
		if err != nil {
			return err
		}
		tf, td, err := ParseDirTree(treeData)
		if err != nil {
			return err
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			return err
		}
		dirs = append(dirs, dirMode{path, mode})
		for _, f := range tf {
			files = append(files, fileJob{filepath.Join(path, f.Name), f.Checksum})
		}
		for _, d := range td {
			if err := walk(filepath.Join(path, d.Name), d.Tree, d.Meta, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(dir, commit.RootTree, commit.RootMeta, 0); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
		sem      = make(chan struct{}, fileJobs)
	)
	for _, f := range files {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return
			}
			if err := checkoutFile(ctx, src, f.checksum, f.path); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	// Directory modes last, deepest first, once nothing is written into
	// them any more.
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Chmod(dirs[i].path, fs.FileMode(dirs[i].mode&0o777|0o700)); err != nil {
			return err
		}
	}
	return nil
}

// checkoutFile fetches a .filez, writes its file to path and checks the
// file's checksum.
func checkoutFile(ctx context.Context, src Source, checksum, path string) error {
	objPath := ObjectPath(checksum, "filez")
	r, err := src.Open(ctx, objPath)
	if errors.Is(err, ErrNotFound) {
		return errNoLooseObjects(objPath)
	}
	if err != nil {
		return err
	}
	defer r.Close()
	h, content, err := readFilez(r)
	if err != nil {
		return fmt.Errorf("%s: %w", objPath, err)
	}
	hash := sha256.New()
	header, err := h.ChecksumHeader()
	if err != nil {
		return fmt.Errorf("%s: %w", objPath, err)
	}
	hash.Write(header)
	if h.Mode&modeType == modeSymlink {
		if got := hex.EncodeToString(hash.Sum(nil)); got != checksum {
			return fmt.Errorf("%s: file checksum is %s", objPath, got)
		}
		return os.Symlink(h.SymlinkTarget, path)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	var n int64
	if h.Size > 0 { // an empty file may come without a deflate stream
		n, err = io.Copy(io.MultiWriter(f, hash), io.LimitReader(content, int64(h.Size)+1))
	}
	if err == nil && uint64(n) != h.Size {
		err = fmt.Errorf("%s: %d bytes of content, the header says %d", objPath, n, h.Size)
	}
	if err == nil {
		if got := hex.EncodeToString(hash.Sum(nil)); got != checksum {
			err = fmt.Errorf("%s: file checksum is %s", objPath, got)
		}
	}
	if err == nil {
		err = f.Chmod(fs.FileMode(h.Mode&0o777 | 0o600))
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// readFilez reads the header of a .filez and returns the stream of the
// uncompressed content after it.
func readFilez(r io.Reader) (FileHeader, io.Reader, error) {
	var prefix [8]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return FileHeader{}, nil, fmt.Errorf("reading the header: %w", err)
	}
	n, err := HeaderSize(prefix)
	if err != nil {
		return FileHeader{}, nil, err
	}
	header := make([]byte, n)
	if _, err := io.ReadFull(r, header); err != nil {
		return FileHeader{}, nil, fmt.Errorf("reading the header: %w", err)
	}
	h, err := ParseZlibHeader(header)
	if err != nil {
		return FileHeader{}, nil, err
	}
	if h.Mode&modeType == modeSymlink {
		return h, nil, nil
	}
	return h, flate.NewReader(r), nil
}
