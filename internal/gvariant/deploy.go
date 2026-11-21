package gvariant

import (
	"fmt"
)

// DeployDataType is the type of the deploy file flatpak writes next to every
// deployment (FLATPAK_DEPLOY_DATA_GVARIANT_STRING in
// common/flatpak-dir-private.h): origin, commit, subpaths, installed size
// (big endian), and a metadata a{sv} built by flatpak_dir_new_deploy_data()
// in common/flatpak-dir.c.
const DeployDataType = "(ssasta{sv})"

// deployFrames is the number of framing offsets of DeployDataType: one for
// each member of variable size but the last (s, s, as); t is fixed and the
// a{sv} is last.
const deployFrames = 3

// DeployData is a parsed deploy file.
type DeployData struct {
	Origin, Commit string
	// Metadata is the entries of the a{sv}, in their order.
	Metadata []Entry
	// prefix is everything before the a{sv}: the three strings, the
	// subpaths, padding and the installed size, as read.
	prefix []byte
	// frames are the framing offsets of the tuple, as read: the ends of
	// origin, commit and subpaths. They do not depend on the a{sv}.
	frames [deployFrames]uint64
}

// ParseDeployData reads a deploy file, checking the a{sv} the way ParseDict
// does. The members before it are kept as bytes; only origin and commit
// are read.
func ParseDeployData(data []byte) (*DeployData, error) {
	members, err := Value{Type: DeployDataType, Data: data}.Members()
	if err != nil {
		return nil, fmt.Errorf("deploy data: %w", err)
	}
	d := &DeployData{}
	if d.Origin, err = members[0].String(); err != nil {
		return nil, fmt.Errorf("deploy data origin: %w", err)
	}
	if d.Commit, err = members[1].String(); err != nil {
		return nil, fmt.Errorf("deploy data commit: %w", err)
	}
	offsetSize := offsetSizeFor(len(data))
	for i := range d.frames {
		at := len(data) - (i+1)*offsetSize
		d.frames[i] = readOffset(data[at : at+offsetSize])
	}
	// The a{sv} starts after the t, aligned to 8, and runs up to the
	// framing offsets (Members has checked the bounds).
	start := alignUp(int(d.frames[2]), 8) + 8
	start = alignUp(start, entryAlign)
	end := len(data) - deployFrames*offsetSize
	if start > end {
		return nil, fmt.Errorf("deploy data: %w", notNormal("metadata out of bounds"))
	}
	for i := int(d.frames[2]); i < alignUp(int(d.frames[2]), 8); i++ {
		if data[i] != 0 {
			return nil, fmt.Errorf("deploy data: %w", notNormal("padding"))
		}
	}
	if totalSize(end, deployFrames) != len(data) {
		return nil, fmt.Errorf("deploy data: %w", notNormal("offset size is not minimal"))
	}
	if d.Metadata, err = ParseDict(data[start:end]); err != nil {
		return nil, fmt.Errorf("deploy data metadata: %w", err)
	}
	d.prefix = data[:start]
	return d, nil
}

// Serialize writes the deploy data back: the bytes before the a{sv} as they
// were read, the a{sv} from Metadata, and the framing, whose offsets do not
// change but whose size may.
func (d *DeployData) Serialize() []byte {
	dict := SerializeDict(d.Metadata)
	body := len(d.prefix) + len(dict)
	size := totalSize(body, deployFrames)
	offsetSize := offsetSizeFor(size)
	out := make([]byte, 0, size)
	out = append(out, d.prefix...)
	out = append(out, dict...)
	// Framing offsets are stored last member first: the end of the
	// subpaths, then of commit, then of origin, which is at the very end.
	for i := deployFrames - 1; i >= 0; i-- {
		out = appendOffset(out, d.frames[i], offsetSize)
	}
	return out
}

// String returns the value of a string entry of the metadata, the last
// one if the key is there more than once, as a lookup in GLib finds it.
func (d *DeployData) String(key string) (string, bool) {
	value, ok := "", false
	for _, e := range d.Metadata {
		if e.Key != key || e.Type != "s" {
			continue
		}
		v, err := Value{Type: "{sv}", Data: e.Raw}.Members()
		if err != nil {
			continue
		}
		inner, err := v[1].Variant()
		if err != nil {
			continue
		}
		if s, err := inner.String(); err == nil {
			value, ok = s, true
		}
	}
	return value, ok
}

// SetString replaces the value of every entry with key by a string. It
// reports whether there was such an entry; a missing key is not added.
func (d *DeployData) SetString(key, value string) bool {
	found := false
	for i, e := range d.Metadata {
		if e.Key == key {
			d.Metadata[i] = StringEntry(key, value)
			found = true
		}
	}
	return found
}

// StringEntry is the dict entry {sv} of key with a string value.
func StringEntry(key, value string) Entry {
	raw := append([]byte(key), 0)
	keyEnd := len(raw)
	for len(raw)%entryAlign != 0 {
		raw = append(raw, 0)
	}
	// The variant: the string with its NUL, a NUL, the type string.
	raw = append(raw, value...)
	raw = append(raw, 0, 0, 's')
	raw = appendOffset(raw, uint64(keyEnd), offsetSizeFor(totalSize(len(raw), 1)))
	return Entry{Key: key, Type: "s", Raw: raw}
}

// SerializeArray joins serialised elements of a type of variable size and
// the given alignment into an array in normal form.
func SerializeArray(elements [][]byte, align int) []byte {
	if len(elements) == 0 {
		return []byte{}
	}
	body := 0
	for _, e := range elements {
		body = alignUp(body, align) + len(e)
	}
	size := totalSize(body, len(elements))
	offsetSize := offsetSizeFor(size)
	out := make([]byte, 0, size)
	ends := make([]int, 0, len(elements))
	for _, e := range elements {
		for len(out)%align != 0 {
			out = append(out, 0)
		}
		out = append(out, e...)
		ends = append(ends, len(out))
	}
	for _, end := range ends {
		out = appendOffset(out, uint64(end), offsetSize)
	}
	return out
}

// FrameTuple completes the body of a tuple (its members, aligned) with its
// framing: ends are the end offsets of its members of variable size but
// the last, in member order; they are stored the other way round, the
// first member's last, in the smallest size that can address the result.
func FrameTuple(body []byte, ends ...uint64) []byte {
	size := totalSize(len(body), len(ends))
	offsetSize := offsetSizeFor(size)
	out := make([]byte, 0, size)
	out = append(out, body...)
	for i := len(ends) - 1; i >= 0; i-- {
		out = appendOffset(out, ends[i], offsetSize)
	}
	return out
}
