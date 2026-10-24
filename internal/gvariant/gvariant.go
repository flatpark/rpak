// Package gvariant reads and writes just enough of the GVariant
// serialisation format to remove entries from an a{sv} dictionary, such as
// ostree's detached commit metadata, without understanding their values.
//
// The format is the one of GLib's gvariant-serialiser.c (little endian,
// normal form only):
//
//   - An array of variable-sized elements (a{sv}) is the elements one after
//     another, each starting at a multiple of the element alignment (8 for
//     {sv}, the alignment of v) with zero padding in between, followed by
//     the end offset of every element. An empty array is empty.
//   - A dict entry {sv} is the key (a string: its bytes and a NUL), zero
//     padding up to a multiple of 8, the value, and at the very end the
//     offset where the key ends. The value, being the last member, has no
//     offset of its own.
//   - A variant (v) is the value's bytes, a NUL, and the value's type string.
//   - Framing offsets are little endian, all of the same size in a
//     container: the smallest of 1, 2, 4 or 8 bytes for which the container,
//     offsets included, is no bigger than that size can address
//     (gvs_calculate_total_size). A reader derives the size from the size of
//     the container (gvs_get_offset_size).
//
// Entries are kept as the exact bytes they were read as: every dict entry
// starts at a multiple of 8 and alignment inside it is relative to that,
// so an entry's bytes do not depend on where in the array it ends up. Only
// the array framing is computed anew.
package gvariant

import (
	"bytes"
	"errors"
	"fmt"
	"unicode/utf8"
)

// ErrNotNormal is returned for input that GLib would not have produced:
// anything that is not an a{sv} in normal form, as far as this package
// checks it (see ParseDict).
var ErrNotNormal = errors.New("not an a{sv} GVariant in normal form")

// Entry is one {sv} of a dictionary.
type Entry struct {
	Key string
	// Type is the type string of the value.
	Type string
	// Raw is the serialised dict entry: key, value and framing.
	Raw []byte
}

// entryAlign is the alignment of {sv}: that of v, 8.
const entryAlign = 8

// ParseDict splits a serialised a{sv} into its entries, checking that the
// array and every entry are in normal form: minimal offset sizes,
// offsets in order and in bounds, zero padding, keys that are NUL-terminated
// UTF-8 without other NULs, and variants whose type string is one definite
// type, of the right size if that type has a fixed size. What it does not
// check is the content of the values: they are copied, never interpreted.
func ParseDict(data []byte) ([]Entry, error) {
	if len(data) == 0 {
		return nil, nil
	}
	offsetSize := offsetSizeFor(len(data))
	lastEnd := readOffset(data[len(data)-offsetSize:])
	if lastEnd > uint64(len(data)) {
		return nil, notNormal("array offsets out of bounds")
	}
	tableSize := len(data) - int(lastEnd)
	if tableSize%offsetSize != 0 || tableSize == 0 {
		return nil, notNormal("array offset table size")
	}
	n := tableSize / offsetSize
	if totalSize(int(lastEnd), n) != len(data) {
		return nil, notNormal("array offset size is not minimal")
	}
	table := data[lastEnd:]
	entries := make([]Entry, 0, n)
	offset := 0
	for i := 0; i < n; i++ {
		end := readOffset(table[i*offsetSize : (i+1)*offsetSize])
		if end < uint64(offset) || end > lastEnd {
			return nil, notNormal("array offsets not in order")
		}
		for offset%entryAlign != 0 {
			if uint64(offset) >= end || data[offset] != 0 {
				return nil, notNormal("array padding")
			}
			offset++
		}
		entry, err := parseEntry(data[offset:end])
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
		offset = int(end)
	}
	return entries, nil
}

func parseEntry(raw []byte) (Entry, error) {
	// gvs_tuple_is_normal for (s, v): the key ends at the offset stored at
	// the end, the value runs up to that offset.
	if len(raw) == 0 {
		return Entry{}, notNormal("empty dict entry")
	}
	offsetSize := offsetSizeFor(len(raw))
	if len(raw) < offsetSize {
		return Entry{}, notNormal("dict entry too short")
	}
	valueEnd := len(raw) - offsetSize
	keyEnd := readOffset(raw[valueEnd:])
	if keyEnd > uint64(valueEnd) {
		return Entry{}, notNormal("key offset out of bounds")
	}
	key, err := parseString(raw[:keyEnd])
	if err != nil {
		return Entry{}, err
	}
	offset := int(keyEnd)
	for offset%entryAlign != 0 {
		if offset >= len(raw) || raw[offset] != 0 {
			return Entry{}, notNormal("dict entry padding")
		}
		offset++
	}
	if offset > valueEnd {
		return Entry{}, notNormal("value out of bounds")
	}
	typ, err := parseVariant(raw[offset:valueEnd])
	if err != nil {
		return Entry{}, fmt.Errorf("value of %q: %w", key, err)
	}
	if totalSize(valueEnd, 1) != len(raw) {
		return Entry{}, notNormal("dict entry offset size is not minimal")
	}
	return Entry{Key: key, Type: typ, Raw: raw}, nil
}

// parseString checks a serialised string (gvs_string_is_normal).
func parseString(b []byte) (string, error) {
	if len(b) == 0 || b[len(b)-1] != 0 {
		return "", notNormal("string not NUL-terminated")
	}
	s := b[:len(b)-1]
	if bytes.IndexByte(s, 0) >= 0 || !utf8.Valid(s) {
		return "", notNormal("string with NUL or invalid UTF-8")
	}
	return string(s), nil
}

// parseVariant checks the framing of a serialised variant and returns the
// type string of its value (gvs_variant_get_child, gvs_variant_is_normal).
func parseVariant(b []byte) (string, error) {
	nul := bytes.LastIndexByte(b, 0)
	if nul < 0 {
		return "", notNormal("variant without type string")
	}
	typ := string(b[nul+1:])
	size, err := fixedSize(typ)
	if err != nil {
		return "", err
	}
	if size != 0 && size != nul {
		return "", notNormal("variant value of the wrong size")
	}
	return typ, nil
}

// fixedSize returns the fixed size of a definite type, 0 for types of
// variable size, and an error if typ is not exactly one definite type.
func fixedSize(typ string) (int, error) {
	size, _, rest, err := scanType(typ, 0)
	if err != nil {
		return 0, err
	}
	if rest != "" {
		return 0, notNormal("trailing characters in type string")
	}
	return size, nil
}

// maxDepth bounds type nesting like G_VARIANT_MAX_RECURSION_DEPTH.
const maxDepth = 128

// scanType reads one complete definite type from the start of typ and
// returns its fixed size (0 if variable), its alignment and what follows.
func scanType(typ string, depth int) (size, align int, rest string, err error) {
	if depth > maxDepth {
		return 0, 0, "", notNormal("type nested too deeply")
	}
	if typ == "" {
		return 0, 0, "", notNormal("incomplete type string")
	}
	c, rest := typ[0], typ[1:]
	switch c {
	case 'b', 'y':
		return 1, 1, rest, nil
	case 'n', 'q':
		return 2, 2, rest, nil
	case 'i', 'u', 'h':
		return 4, 4, rest, nil
	case 'x', 't', 'd':
		return 8, 8, rest, nil
	case 's', 'o', 'g':
		return 0, 1, rest, nil
	case 'v':
		return 0, 8, rest, nil
	case 'a', 'm':
		_, align, rest, err := scanType(rest, depth+1)
		return 0, align, rest, err
	case '(', '{':
		closer := byte(')')
		if c == '{' {
			closer = '}'
		}
		// Tuple layout as in g_variant_type_info: members aligned, the
		// size rounded up to the alignment; () has size 1.
		offset, align, fixed, members := 0, 1, true, 0
		for {
			if rest == "" {
				return 0, 0, "", notNormal("unterminated tuple type")
			}
			if rest[0] == closer {
				rest = rest[1:]
				break
			}
			if c == '{' && members == 0 && !basicType(rest[0]) {
				return 0, 0, "", notNormal("dict entry key is not a basic type")
			}
			var size, a int
			size, a, rest, err = scanType(rest, depth+1)
			if err != nil {
				return 0, 0, "", err
			}
			members++
			align = max(align, a)
			offset = alignUp(offset, a) + size
			if size == 0 {
				fixed = false
			}
		}
		if c == '{' && members != 2 {
			return 0, 0, "", notNormal("dict entry type without two members")
		}
		if members == 0 {
			return 1, 1, rest, nil
		}
		if !fixed {
			return 0, align, rest, nil
		}
		return alignUp(offset, align), align, rest, nil
	default:
		// Includes the indefinite types r, *, ? and "{" outside arrays,
		// which GLib does accept; it never matters for a{sv} values.
		return 0, 0, "", notNormal(fmt.Sprintf("unsupported type character %q", c))
	}
}

func basicType(c byte) bool {
	return bytes.IndexByte([]byte("bynqiuxtdhsog"), c) >= 0
}

func alignUp(n, align int) int {
	return (n + align - 1) / align * align
}

// SerializeDict joins entries (as returned by ParseDict) into an a{sv} in
// normal form, in the given order.
func SerializeDict(entries []Entry) []byte {
	if len(entries) == 0 {
		return []byte{}
	}
	body := 0
	for _, e := range entries {
		body = alignUp(body, entryAlign) + len(e.Raw)
	}
	size := totalSize(body, len(entries))
	offsetSize := offsetSizeFor(size)
	out := make([]byte, 0, size)
	ends := make([]int, 0, len(entries))
	for _, e := range entries {
		for len(out)%entryAlign != 0 {
			out = append(out, 0)
		}
		out = append(out, e.Raw...)
		ends = append(ends, len(out))
	}
	for _, end := range ends {
		out = appendOffset(out, uint64(end), offsetSize)
	}
	return out
}

// RemoveKey removes every entry with the given key from the serialised
// a{sv} data. It reports whether there was one; if not, out is data.
// Input that is not in normal form, or that has some other key more than
// once, is refused, so that nothing that was not fully understood is
// rewritten.
func RemoveKey(data []byte, key string) (out []byte, removed bool, err error) {
	entries, err := ParseDict(data)
	if err != nil {
		return nil, false, err
	}
	seen := make(map[string]bool, len(entries))
	var kept []Entry
	var duplicate string
	for _, e := range entries {
		if e.Key == key {
			removed = true
			continue
		}
		if seen[e.Key] && duplicate == "" {
			duplicate = e.Key
		}
		seen[e.Key] = true
		kept = append(kept, e)
	}
	if !removed {
		return data, false, nil
	}
	if duplicate != "" {
		return nil, false, fmt.Errorf("key %q appears more than once", duplicate)
	}
	return SerializeDict(kept), true, nil
}

// offsetSizeFor is gvs_get_offset_size: the size of the framing offsets
// of a container of the given size.
func offsetSizeFor(size int) int {
	switch {
	case uint64(size) > 0xffffffff:
		return 8
	case size > 0xffff:
		return 4
	case size > 0xff:
		return 2
	case size > 0:
		return 1
	}
	return 0
}

// totalSize is gvs_calculate_total_size: the size of a container with a
// body of bodySize bytes and n framing offsets of the smallest size that
// can address it.
func totalSize(bodySize, n int) int {
	for _, s := range []int{1, 2, 4} {
		if uint64(bodySize+s*n) <= 1<<(8*s)-1 {
			return bodySize + s*n
		}
	}
	return bodySize + 8*n
}

func readOffset(b []byte) uint64 {
	var v uint64
	for i := len(b) - 1; i >= 0; i-- {
		v = v<<8 | uint64(b[i])
	}
	return v
}

func appendOffset(out []byte, v uint64, size int) []byte {
	for i := 0; i < size; i++ {
		out = append(out, byte(v>>(8*i)))
	}
	return out
}

func notNormal(what string) error {
	return fmt.Errorf("%w: %s", ErrNotNormal, what)
}
