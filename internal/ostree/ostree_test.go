package ostree

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/flatpark/rpak/internal/gvariant"
)

// The fixture is a repository ostree itself committed, see testdata/gen.sh.

func fixtureCommit(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/COMMIT")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func fixtureSource(t *testing.T) Source {
	t.Helper()
	abs, err := filepath.Abs("testdata/repo")
	if err != nil {
		t.Fatal(err)
	}
	src, err := NewSource("file://"+abs, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	return src
}

// TestFileChecksums recomputes the checksum of every file object of the
// fixture, which ostree named by it: header and content as ostree hashes
// them.
func TestFileChecksums(t *testing.T) {
	paths, err := filepath.Glob("testdata/repo/objects/*/*.filez")
	if err != nil || len(paths) < 6 {
		t.Fatalf("%d file objects (%v); run testdata/gen.sh", len(paths), err)
	}
	kinds := map[uint32]int{}
	sawXattrs := false
	for _, p := range paths {
		checksum := filepath.Base(filepath.Dir(p)) + strings.TrimSuffix(filepath.Base(p), ".filez")
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		h, content, err := readFilez(f)
		if err != nil {
			t.Fatalf("%s: %v", checksum, err)
		}
		kinds[h.Mode&modeType]++
		if len(h.xattrs) > 0 {
			sawXattrs = true
		}
		header, err := h.ChecksumHeader()
		if err != nil {
			t.Fatal(err)
		}
		var data []byte
		if content != nil && h.Size > 0 {
			if data, err = io.ReadAll(content); err != nil {
				t.Fatalf("%s: %v", checksum, err)
			}
		}
		f.Close()
		if uint64(len(data)) != h.Size {
			t.Errorf("%s: %d bytes, header says %d", checksum, len(data), h.Size)
		}
		hash := sha256Hex(append(header, data...))
		if hash != checksum {
			t.Errorf("%s: computed %s (mode %o)", checksum, hash, h.Mode)
		}
	}
	if kinds[modeRegular] == 0 || kinds[modeSymlink] == 0 {
		t.Errorf("fixture lacks regular files or symlinks: %v", kinds)
	}
	if !sawXattrs {
		t.Log("fixture has no xattrs (the file system of gen.sh did not take them)")
	}
}

func TestCanonicalXattrs(t *testing.T) {
	elem := func(name, value string) []byte {
		n := append([]byte(name), 0)
		return gvariant.FrameTuple(append(append([]byte{}, n...), value...), uint64(len(n)))
	}
	sorted := gvariant.SerializeArray([][]byte{elem("user.aa", "2"), elem("user.zz", "1")}, 1)
	unsorted := gvariant.SerializeArray([][]byte{elem("user.zz", "1"), elem("user.aa", "2")}, 1)
	got, err := canonicalXattrs(unsorted)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, sorted) {
		t.Errorf("got %x, want %x", got, sorted)
	}
	if got, _ := canonicalXattrs(sorted); !bytes.Equal(got, sorted) {
		t.Errorf("sorted input changed")
	}
	if got, _ := canonicalXattrs(nil); len(got) != 0 {
		t.Errorf("empty: %x", got)
	}
}

func checkTree(t *testing.T, dir string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "files/bin/hello"))
	if err != nil || string(b) != "#!/bin/sh\necho hello\n" {
		t.Errorf("hello: %q %v", b, err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "files/bin/hello")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("hello mode: %v %v", fi.Mode(), err)
	}
	if target, err := os.Readlink(filepath.Join(dir, "files/bin/link")); err != nil || target != "../share/doc/big.txt" {
		t.Errorf("link: %q %v", target, err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "files/bin/link")); err != nil || len(b) != 2000*len("compressible line\n") {
		t.Errorf("big.txt through the link: %d bytes, %v", len(b), err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "files/share/doc/empty")); err != nil || fi.Size() != 0 {
		t.Errorf("empty: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "files/share")); err != nil || fi.Mode().Perm() != 0o750 {
		t.Errorf("share mode: %v %v", fi.Mode(), err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "export")); err != nil || !fi.IsDir() {
		t.Errorf("export: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "metadata")); err != nil || !strings.Contains(string(b), "name=org.example.Fixture") {
		t.Errorf("metadata: %q %v", b, err)
	}
}

func TestCheckoutFile(t *testing.T) {
	src := fixtureSource(t)
	commit, err := ReadCommit(context.Background(), src, fixtureCommit(t))
	if err != nil {
		t.Fatal(err)
	}
	if commit.Subject != "fixture" || commit.Body != "rpak.upstream=x" {
		t.Errorf("subject %q body %q", commit.Subject, commit.Body)
	}
	dir := filepath.Join(t.TempDir(), "co")
	if err := Checkout(context.Background(), src, commit, dir); err != nil {
		t.Fatal(err)
	}
	checkTree(t, dir)
}

func TestCheckoutHTTP(t *testing.T) {
	var requests atomic.Int64 // objects are fetched concurrently
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("User-Agent") != "rpak-test" {
			t.Errorf("User-Agent %q", r.Header.Get("User-Agent"))
		}
		http.FileServer(http.Dir("testdata/repo")).ServeHTTP(w, r)
	}))
	defer srv.Close()
	src, err := NewSource(srv.URL+"/", srv.Client(), "rpak-test")
	if err != nil {
		t.Fatal(err)
	}
	commit, err := ReadCommit(context.Background(), src, fixtureCommit(t))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "co")
	if err := Checkout(context.Background(), src, commit, dir); err != nil {
		t.Fatal(err)
	}
	checkTree(t, dir)
	if requests.Load() < 10 {
		t.Errorf("only %d requests", requests.Load())
	}
}

// corruptSource serves the fixture with one object changed or missing.
type corruptSource struct {
	Source
	suffix  string
	missing bool
}

func (c corruptSource) Open(ctx context.Context, path string) (io.ReadCloser, error) {
	if !strings.HasSuffix(path, c.suffix) {
		return c.Source.Open(ctx, path)
	}
	if c.missing {
		return nil, ErrNotFound
	}
	r, err := c.Source.Open(ctx, path)
	if err != nil {
		return nil, err
	}
	data, _ := io.ReadAll(r)
	r.Close()
	data[len(data)-1] ^= 1
	return io.NopCloser(bytes.NewReader(data)), nil
}

func TestCheckoutRefusesBadObjects(t *testing.T) {
	src := fixtureSource(t)
	commitSum := fixtureCommit(t)
	commit, err := ReadCommit(context.Background(), src, commitSum)
	if err != nil {
		t.Fatal(err)
	}
	// Another checksum than the commit's.
	other := strings.Repeat("0", 64)
	if _, err := ReadCommit(context.Background(), src, other); err == nil {
		t.Error("a missing commit was read")
	}
	if _, err := ReadCommit(context.Background(), corruptSource{src, ".commit", false}, commitSum); err == nil {
		t.Error("a changed commit was accepted")
	}
	// The biggest file, so that the change is in its compressed content.
	big := biggestFilez(t)
	for _, c := range []corruptSource{
		{src, ".dirtree", false},
		{src, ".dirmeta", false},
		{src, big, false},
		{src, big, true},
	} {
		dir := filepath.Join(t.TempDir(), "co")
		err := Checkout(context.Background(), c, commit, dir)
		if err == nil {
			t.Errorf("%s (missing %v): no error", c.suffix, c.missing)
			continue
		}
		if c.missing && !strings.Contains(err.Error(), "static deltas") {
			t.Errorf("missing object: %v", err)
		}
	}
}

func biggestFilez(t *testing.T) string {
	paths, _ := filepath.Glob("testdata/repo/objects/*/*.filez")
	var best string
	var size int64
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil && fi.Size() > size {
			best, size = p, fi.Size()
		}
	}
	return filepath.Base(best)
}

func TestHeaderSize(t *testing.T) {
	var p [8]byte
	binary.BigEndian.PutUint32(p[:], 40)
	if n, err := HeaderSize(p); err != nil || n != 40 {
		t.Errorf("%d %v", n, err)
	}
	p[5] = 1
	if _, err := HeaderSize(p); err == nil {
		t.Error("non-zero padding accepted")
	}
	if _, err := HeaderSize([8]byte{}); err == nil {
		t.Error("size 0 accepted")
	}
}

func TestNewSource(t *testing.T) {
	if _, err := NewSource("oci+https://example.org", nil, ""); err == nil {
		t.Error("oci+https accepted")
	}
	src, _ := NewSource("file:///nonexistent", nil, "")
	if _, err := src.Open(context.Background(), "config"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing file: %v", err)
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
