// Package ostree reads the objects of an ostree repository in archive mode
// (what flatpak remotes serve) and checks them against their checksums, as
// libostree does (src/libostree/ostree-core.c of ostree 2025/2026):
//
//   - Metadata objects (commit, dirtree, dirmeta) are GVariants stored as
//     they are; their checksum is the SHA-256 of those bytes.
//   - A file object, .filez in an archive repository, is a header and the
//     file's content compressed with raw deflate. The header is a
//     (tuuuusa(ayay)) (size, uid, gid, mode, rdev, symlink target, xattrs;
//     numbers big endian), prefixed with its size as a big-endian uint32
//     and four NULs (_ostree_zlib_file_header_new(),
//     variant_to_lenprefixed_buffer()).
//   - A file's checksum is not that of the .filez: it is the SHA-256 of the
//     uncompressed header (uuuusa(ayay)) without the size, rdev 0, xattrs
//     sorted by name, length-prefixed the same way, followed by the
//     content of a regular file (_ostree_file_header_new(),
//     ostree_checksum_file_from_input()).
//
// Object paths are objects/XX/REST.TYPE for a checksum XXREST.
package ostree

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/flatpark/rpak/internal/gvariant"
)

// GVariant types of the metadata objects (ostree-core.h).
const (
	CommitType  = "(a{sv}aya(say)sstayay)"
	DirTreeType = "(a(say)a(sayay))"
	DirMetaType = "(uuua(ayay))"
	// zlibHeaderType is the header of a .filez, fileHeaderType what the
	// checksum covers.
	zlibHeaderType = "(tuuuusa(ayay))"
)

// File types in st_mode.
const (
	modeType    = 0o170000
	modeRegular = 0o100000
	modeSymlink = 0o120000
	modeDir     = 0o040000
)

// ObjectPath is the path of an object in a repository.
func ObjectPath(checksum, kind string) string {
	return "objects/" + checksum[:2] + "/" + checksum[2:] + "." + kind
}

// ValidChecksum is ostree_validate_checksum_string(): 64 lowercase hex.
func ValidChecksum(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

// verifyMeta checks that data is the metadata object checksum.
func verifyMeta(data []byte, checksum string) error {
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != checksum {
		return fmt.Errorf("object %s has checksum %s", checksum, got)
	}
	return nil
}

// Commit is the part of a commit object that matters here.
type Commit struct {
	// Metadata is the commit's a{sv}: xa.metadata and the like.
	Metadata           gvariant.Value
	Subject, Body      string
	RootTree, RootMeta string
}

// ParseCommit reads a commit object.
func ParseCommit(data []byte) (Commit, error) {
	m, err := gvariant.Value{Type: CommitType, Data: data}.Members()
	if err != nil {
		return Commit{}, fmt.Errorf("commit: %w", err)
	}
	c := Commit{Metadata: m[0]}
	if c.Subject, err = m[3].String(); err != nil {
		return Commit{}, fmt.Errorf("commit subject: %w", err)
	}
	if c.Body, err = m[4].String(); err != nil {
		return Commit{}, fmt.Errorf("commit body: %w", err)
	}
	if c.RootTree, err = checksumOf(m[6]); err != nil {
		return Commit{}, fmt.Errorf("commit root tree: %w", err)
	}
	if c.RootMeta, err = checksumOf(m[7]); err != nil {
		return Commit{}, fmt.Errorf("commit root meta: %w", err)
	}
	return c, nil
}

// MetadataString is a string in the commit's metadata, such as
// xa.metadata.
func (c Commit) MetadataString(key string) (string, bool, error) {
	v, ok, err := c.Metadata.Lookup(key)
	if err != nil || !ok {
		return "", false, err
	}
	inner, err := v.Variant()
	if err != nil {
		return "", false, err
	}
	s, err := inner.String()
	if err != nil {
		return "", false, err
	}
	return s, true, nil
}

func checksumOf(v gvariant.Value) (string, error) {
	b, err := v.Bytes()
	if err != nil {
		return "", err
	}
	if len(b) != sha256.Size {
		return "", fmt.Errorf("checksum of %d bytes", len(b))
	}
	return hex.EncodeToString(b), nil
}

// TreeFile and TreeDir are the entries of a dirtree.
type TreeFile struct{ Name, Checksum string }
type TreeDir struct{ Name, Tree, Meta string }

// ParseDirTree reads a dirtree object.
func ParseDirTree(data []byte) ([]TreeFile, []TreeDir, error) {
	m, err := gvariant.Value{Type: DirTreeType, Data: data}.Members()
	if err != nil {
		return nil, nil, fmt.Errorf("dirtree: %w", err)
	}
	fileEntries, err := m[0].Elements()
	if err != nil {
		return nil, nil, fmt.Errorf("dirtree: %w", err)
	}
	dirEntries, err := m[1].Elements()
	if err != nil {
		return nil, nil, fmt.Errorf("dirtree: %w", err)
	}
	seen := map[string]bool{}
	name := func(v gvariant.Value) (string, error) {
		n, err := v.String()
		if err != nil {
			return "", err
		}
		// ostree_validate_filename(), and no name twice.
		if n == "" || n == "." || n == ".." || strings.Contains(n, "/") || seen[n] {
			return "", fmt.Errorf("dirtree: bad file name %q", n)
		}
		seen[n] = true
		return n, nil
	}
	var files []TreeFile
	for _, e := range fileEntries {
		fm, err := e.Members()
		if err != nil {
			return nil, nil, fmt.Errorf("dirtree: %w", err)
		}
		n, err := name(fm[0])
		if err != nil {
			return nil, nil, err
		}
		sum, err := checksumOf(fm[1])
		if err != nil {
			return nil, nil, fmt.Errorf("dirtree %s: %w", n, err)
		}
		files = append(files, TreeFile{n, sum})
	}
	var dirs []TreeDir
	for _, e := range dirEntries {
		dm, err := e.Members()
		if err != nil {
			return nil, nil, fmt.Errorf("dirtree: %w", err)
		}
		n, err := name(dm[0])
		if err != nil {
			return nil, nil, err
		}
		tree, err := checksumOf(dm[1])
		if err != nil {
			return nil, nil, fmt.Errorf("dirtree %s: %w", n, err)
		}
		meta, err := checksumOf(dm[2])
		if err != nil {
			return nil, nil, fmt.Errorf("dirtree %s: %w", n, err)
		}
		dirs = append(dirs, TreeDir{n, tree, meta})
	}
	return files, dirs, nil
}

// ParseDirMeta returns the mode of a dirmeta object.
func ParseDirMeta(data []byte) (uint32, error) {
	m, err := gvariant.Value{Type: DirMetaType, Data: data}.Members()
	if err != nil {
		return 0, fmt.Errorf("dirmeta: %w", err)
	}
	mode := binary.BigEndian.Uint32(m[2].Data)
	if mode&modeType != modeDir {
		return 0, fmt.Errorf("dirmeta: mode %o is not a directory", mode)
	}
	return mode, nil
}

// FileHeader is the header of a .filez.
type FileHeader struct {
	Size          uint64
	UID, GID      uint32
	Mode          uint32
	SymlinkTarget string
	// xattrs is the serialised a(ayay), as stored.
	xattrs []byte
}

// maxHeader bounds the header of a .filez; ostree's own limit for a
// variant read from a stream is 10 MB (_ostree_read_variant), headers are
// a few dozen bytes.
const maxHeader = 1 << 20

// ParseZlibHeader reads the header of a .filez, without its size prefix
// (see HeaderSize).
func ParseZlibHeader(header []byte) (FileHeader, error) {
	m, err := gvariant.Value{Type: zlibHeaderType, Data: header}.Members()
	if err != nil {
		return FileHeader{}, fmt.Errorf("file header: %w", err)
	}
	h := FileHeader{
		Size: binary.BigEndian.Uint64(m[0].Data),
		UID:  binary.BigEndian.Uint32(m[1].Data),
		GID:  binary.BigEndian.Uint32(m[2].Data),
		Mode: binary.BigEndian.Uint32(m[3].Data),
	}
	if h.SymlinkTarget, err = m[5].String(); err != nil {
		return FileHeader{}, fmt.Errorf("file header: %w", err)
	}
	h.xattrs = m[6].Data
	switch h.Mode & modeType {
	case modeRegular:
		if h.SymlinkTarget != "" {
			return FileHeader{}, fmt.Errorf("file header: a regular file with a symlink target")
		}
	case modeSymlink:
		// The size of a symlink is whatever lstat() said, the length of
		// the target; there is no content.
		if h.SymlinkTarget == "" {
			return FileHeader{}, fmt.Errorf("file header: a bad symlink")
		}
	default:
		return FileHeader{}, fmt.Errorf("file header: mode %o is neither a file nor a symlink", h.Mode)
	}
	return h, nil
}

// HeaderSize reads the size prefix of a .filez header.
func HeaderSize(prefix [8]byte) (int, error) {
	n := binary.BigEndian.Uint32(prefix[:4])
	if n == 0 || n > maxHeader || !bytes.Equal(prefix[4:], []byte{0, 0, 0, 0}) {
		return 0, fmt.Errorf("file header: bad size prefix %x", prefix)
	}
	return int(n), nil
}

// ChecksumHeader is what a file's checksum starts with
// (_ostree_file_header_new()): the length-prefixed (uuuusa(ayay)), rdev
// 0, xattrs sorted by name.
func (h FileHeader) ChecksumHeader() ([]byte, error) {
	xattrs, err := canonicalXattrs(h.xattrs)
	if err != nil {
		return nil, err
	}
	body := make([]byte, 16, 16+len(h.SymlinkTarget)+1+len(xattrs))
	binary.BigEndian.PutUint32(body[0:], h.UID)
	binary.BigEndian.PutUint32(body[4:], h.GID)
	binary.BigEndian.PutUint32(body[8:], h.Mode)
	// body[12:16], rdev, stays 0.
	body = append(body, h.SymlinkTarget...)
	body = append(body, 0)
	targetEnd := uint64(len(body))
	// a(ayay) has alignment 1: no padding before it.
	body = append(body, xattrs...)
	v := gvariant.FrameTuple(body, targetEnd)
	out := make([]byte, 8, 8+len(v))
	binary.BigEndian.PutUint32(out, uint32(len(v)))
	return append(out, v...), nil
}

// canonicalXattrs is _ostree_canonicalize_xattrs(): the a(ayay) sorted by
// name (strcmp of the NUL-terminated names).
func canonicalXattrs(raw []byte) ([]byte, error) {
	elems, err := gvariant.Value{Type: "a(ayay)", Data: raw}.Elements()
	if err != nil {
		return nil, fmt.Errorf("xattrs: %w", err)
	}
	type xattr struct {
		name string
		raw  []byte
	}
	xs := make([]xattr, len(elems))
	for i, e := range elems {
		m, err := e.Members()
		if err != nil {
			return nil, fmt.Errorf("xattrs: %w", err)
		}
		name, _, _ := bytes.Cut(m[0].Data, []byte{0})
		xs[i] = xattr{string(name), e.Data}
	}
	if sort.SliceIsSorted(xs, func(i, j int) bool { return xs[i].name < xs[j].name }) {
		return raw, nil
	}
	sort.SliceStable(xs, func(i, j int) bool { return xs[i].name < xs[j].name })
	parts := make([][]byte, len(xs))
	for i, x := range xs {
		parts[i] = x.raw
	}
	return gvariant.SerializeArray(parts, 1), nil
}
