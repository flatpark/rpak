package prefetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/flatpark/rpak/internal/metadata"
)

// Fetch downloads uri as the extra-data file name when its checksum is not
// known yet (rpak --extra-data): the checksum and the size are what came,
// and the file ends up where flatpak looks for it, so that the install
// that follows does not download it again.
//
// The file is downloaded into a marked temporary directory of the
// extra-data directory (tmpPrefix, which expires like any marked one if
// rpak is killed), hashed on the way, synced, and the directory renamed to
// extra-data/<sha256>/: flatpak never sees a file under a checksum it does
// not have. A <sha256>/ directory of rpak's that is there already gets the
// file; one that is not rpak's is left alone (flatpak uses its file or
// downloads it itself). dir is the marked directory now holding the file,
// to be removed like prefetched ones once flatpak has used it, or "".
//
// Every attempt starts over: a part file cannot be checked against
// anything. A response of 4xx is not retried. Callbacks get the source
// with its size as the server announces it (0 if it does not; OnDone has
// the size that came) and no checksum. The caller holds LockShared.
func (d *Downloader) Fetch(ctx context.Context, uri, name string) (src metadata.Source, dir string, err error) {
	if !metadata.ValidName(name) {
		return src, "", fmt.Errorf("invalid extra-data name %q", name)
	}
	root := filepath.Join(d.cfg.BaseDir, "extra-data")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return src, "", err
	}
	tmp, err := os.MkdirTemp(root, tmpPrefix)
	if err != nil {
		return src, "", err
	}
	defer func() {
		if tmp != "" {
			os.RemoveAll(tmp)
		}
	}()
	marker := fmt.Sprintf("rpak %s %s\n", d.cfg.Version, time.Now().UTC().Format(time.RFC3339))
	if err := os.Chmod(tmp, 0o755); err != nil {
		return src, "", err
	}
	if err := os.WriteFile(filepath.Join(tmp, MarkerName), []byte(marker), 0o644); err != nil {
		return src, "", err
	}
	src = metadata.Source{Name: name, URI: uri}
	part := filepath.Join(tmp, name+".part")
	started := false
	for attempt := 0; attempt <= d.cfg.Retries; attempt++ {
		if attempt > 0 {
			if werr := d.wait(ctx, src, err, attempt); werr != nil {
				return src, "", werr
			}
		}
		var sum string
		var size int64
		sum, size, err = d.fetchAttempt(ctx, &src, part, &started)
		if err == nil {
			src.Checksum, src.Size = sum, size
			break
		}
		var fatal *fatalError
		if ctx.Err() != nil || errors.As(err, &fatal) {
			break
		}
	}
	if started && d.cfg.OnDone != nil {
		status := Downloaded
		switch {
		case ctx.Err() != nil:
			status = Canceled
		case err != nil:
			status = Failed
		}
		// Without the checksum, as OnStart had it: callers key on it.
		d.cfg.OnDone(metadata.Source{Name: name, URI: uri, Size: src.Size}, Result{Status: status, Err: err})
	}
	if err != nil {
		var fatal *fatalError
		if errors.As(err, &fatal) {
			err = fatal.err
		}
		return src, "", err
	}
	if err := os.Rename(part, filepath.Join(tmp, name)); err != nil {
		return src, "", err
	}
	if err := syncDir(tmp); err != nil {
		return src, "", err
	}

	dir = ExtraDataDir(d.cfg.BaseDir, src.Checksum)
	err = os.Rename(tmp, dir)
	if err == nil {
		tmp = ""
		return src, dir, syncDir(root)
	}
	// There already, from an earlier run (rpak's) or someone else.
	info, lerr := os.Lstat(dir)
	if lerr != nil {
		return src, "", err
	}
	if !info.IsDir() || !hasMarker(dir) {
		return src, "", nil
	}
	target := filepath.Join(dir, name)
	if verifyFile(target, src.Size, src.Checksum) != nil {
		if err := os.Remove(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return src, "", err
		}
		if err := os.Rename(filepath.Join(tmp, name), target); err != nil {
			return src, "", err
		}
		if err := syncDir(dir); err != nil {
			return src, "", err
		}
	}
	now := time.Now()
	if err := os.Chtimes(filepath.Join(dir, MarkerName), now, now); err != nil {
		return src, "", fmt.Errorf("refresh marker: %w", err)
	}
	return src, dir, nil
}

// fetchAttempt downloads uri to part from scratch and returns its sha256
// and size, synced.
func (d *Downloader) fetchAttempt(ctx context.Context, src *metadata.Source, part string, started *bool) (string, int64, error) {
	f, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.URI, nil)
	if err != nil {
		return "", 0, &fatalError{err}
	}
	req.Header.Set("User-Agent", UserAgent(d.cfg.Version))
	resp, err := d.cfg.Client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	default:
		err := httpError(resp)
		var fatal *fatalError
		if errors.As(err, &fatal) {
			return "", 0, &fatalError{fmt.Errorf("%s: %w", src.URI, fatal.err)}
		}
		return "", 0, fmt.Errorf("%s: %w", src.URI, err)
	}
	if resp.ContentLength > 0 {
		free, err := d.cfg.FreeSpace(filepath.Dir(part))
		if err != nil {
			return "", 0, fmt.Errorf("checking free space: %w", err)
		}
		if free < uint64(resp.ContentLength) {
			return "", 0, &fatalError{fmt.Errorf("not enough free space: need %s, have %s", FormatBytes(resp.ContentLength), FormatBytes(int64(free)))}
		}
		src.Size = resp.ContentLength
	}
	if !*started {
		*started = true
		if d.cfg.OnStart != nil {
			d.cfg.OnStart(*src)
		}
	}

	idle := time.AfterFunc(d.cfg.IdleTimeout, func() {
		cancel(fmt.Errorf("no data received for %s", d.cfg.IdleTimeout))
	})
	defer idle.Stop()
	h := sha256.New()
	w := io.MultiWriter(f, h)
	buf := make([]byte, 256*1024)
	var size int64
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			idle.Reset(d.cfg.IdleTimeout)
			if _, err := w.Write(buf[:n]); err != nil {
				return "", 0, err
			}
			size += int64(n)
			if d.cfg.OnProgress != nil {
				d.cfg.OnProgress(*src, int64(n))
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			if cause := context.Cause(ctx); cause != nil && ctx.Err() != nil && !errors.Is(cause, context.Canceled) {
				return "", 0, cause
			}
			return "", 0, rerr
		}
	}
	if resp.ContentLength >= 0 && size != resp.ContentLength {
		return "", 0, fmt.Errorf("got %d bytes, the server announced %d", size, resp.ContentLength)
	}
	if size == 0 {
		// flatpak build-export takes no extra-data of size 0.
		return "", 0, &fatalError{fmt.Errorf("%s is empty", src.URI)}
	}
	if err := f.Sync(); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}
