package gvariant

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// Value is a serialised GVariant of a known type, for reading any value
// (flatpak's summary files, say) without copying: Members, Elements and
// Variant return views into the same bytes. The layout rules are those in
// the package comment, generalised: in a tuple or dict entry, every member
// starts at a multiple of its alignment, and every member of variable size
// but the last has its end offset in the framing at the end of the
// container, the first member's last. Arrays of fixed-size elements have
// no framing.
//
// It reads normal-form data and refuses anything out of bounds with an
// error; it does not check everything ParseDict checks (padding, for
// one): it is for looking things up in data from elsewhere, not for
// rewriting it.
type Value struct {
	Type string
	Data []byte
}

// splitType returns the first complete type of typ, its fixed size (0 if
// variable), its alignment, and what follows it.
func splitType(typ string) (first string, size, align int, rest string, err error) {
	size, align, rest, err = scanType(typ, 0)
	if err != nil {
		return "", 0, 0, "", err
	}
	return typ[:len(typ)-len(rest)], size, align, rest, nil
}

// Members splits a tuple or dict entry into its members.
func (v Value) Members() ([]Value, error) {
	if len(v.Type) < 2 || (v.Type[0] != '(' && v.Type[0] != '{') {
		return nil, fmt.Errorf("gvariant: %s is not a tuple", v.Type)
	}
	type member struct {
		typ         string
		size, align int
	}
	var members []member
	for inner := v.Type[1 : len(v.Type)-1]; inner != ""; {
		typ, size, align, rest, err := splitType(inner)
		if err != nil {
			return nil, err
		}
		members = append(members, member{typ, size, align})
		inner = rest
	}
	offsetSize := offsetSizeFor(len(v.Data))
	frames := 0 // framing offsets read so far, from the end
	pos := 0
	out := make([]Value, 0, len(members))
	for i, m := range members {
		pos = alignUp(pos, m.align)
		var end int
		switch {
		case m.size > 0:
			end = pos + m.size
		case i == len(members)-1:
			// The last member runs up to the framing offsets of the
			// members before it.
			end = len(v.Data) - frames*offsetSize
		default:
			frames++
			at := len(v.Data) - frames*offsetSize
			if at < 0 {
				return nil, fmt.Errorf("gvariant: %s: framing out of bounds", v.Type)
			}
			end = int(readOffset(v.Data[at : at+offsetSize]))
		}
		if pos > end || end > len(v.Data) {
			return nil, fmt.Errorf("gvariant: %s: member %d out of bounds", v.Type, i)
		}
		out = append(out, Value{m.typ, v.Data[pos:end]})
		pos = end
	}
	return out, nil
}

// Elements splits an array into its elements.
func (v Value) Elements() ([]Value, error) {
	if len(v.Type) < 2 || v.Type[0] != 'a' {
		return nil, fmt.Errorf("gvariant: %s is not an array", v.Type)
	}
	elem, size, align, rest, err := splitType(v.Type[1:])
	if err != nil || rest != "" {
		return nil, fmt.Errorf("gvariant: bad array type %s", v.Type)
	}
	n := len(v.Data)
	if size > 0 {
		if n%size != 0 {
			return nil, fmt.Errorf("gvariant: %s: %d bytes is not a whole number of elements", v.Type, n)
		}
		out := make([]Value, n/size)
		for i := range out {
			out[i] = Value{elem, v.Data[i*size : (i+1)*size]}
		}
		return out, nil
	}
	if n == 0 {
		return nil, nil
	}
	offsetSize := offsetSizeFor(n)
	last := int(readOffset(v.Data[n-offsetSize:]))
	if last < 0 || last > n || (n-last)%offsetSize != 0 {
		return nil, fmt.Errorf("gvariant: %s: bad framing", v.Type)
	}
	count := (n - last) / offsetSize
	out := make([]Value, 0, count)
	start := 0
	for i := 0; i < count; i++ {
		end := int(readOffset(v.Data[last+i*offsetSize : last+(i+1)*offsetSize]))
		start = alignUp(start, align)
		if start > end || end > last {
			return nil, fmt.Errorf("gvariant: %s: element %d out of bounds", v.Type, i)
		}
		out = append(out, Value{elem, v.Data[start:end]})
		start = end
	}
	return out, nil
}

// String is the value of a string (s, o or g).
func (v Value) String() (string, error) {
	if v.Type != "s" && v.Type != "o" && v.Type != "g" {
		return "", fmt.Errorf("gvariant: %s is not a string", v.Type)
	}
	if len(v.Data) == 0 || v.Data[len(v.Data)-1] != 0 || bytes.IndexByte(v.Data[:len(v.Data)-1], 0) >= 0 {
		return "", fmt.Errorf("gvariant: string not NUL-terminated")
	}
	return string(v.Data[:len(v.Data)-1]), nil
}

// Uint32 is the value of a u.
func (v Value) Uint32() (uint32, error) {
	if v.Type != "u" || len(v.Data) != 4 {
		return 0, fmt.Errorf("gvariant: %s is not a uint32", v.Type)
	}
	return binary.LittleEndian.Uint32(v.Data), nil
}

// Bytes is the value of an ay.
func (v Value) Bytes() ([]byte, error) {
	if v.Type != "ay" {
		return nil, fmt.Errorf("gvariant: %s is not a byte array", v.Type)
	}
	return v.Data, nil
}

// Variant is the value inside a v.
func (v Value) Variant() (Value, error) {
	if v.Type != "v" {
		return Value{}, fmt.Errorf("gvariant: %s is not a variant", v.Type)
	}
	i := bytes.LastIndexByte(v.Data, 0)
	if i < 0 {
		return Value{}, fmt.Errorf("gvariant: variant without a type")
	}
	typ := string(v.Data[i+1:])
	size, err := fixedSize(typ)
	if err != nil {
		return Value{}, err
	}
	if size > 0 && i != size {
		return Value{}, fmt.Errorf("gvariant: variant of %s has %d bytes", typ, i)
	}
	return Value{typ, v.Data[:i]}, nil
}

// Lookup finds the value of key in a dictionary with string keys (a{s*}),
// the last one if the key is there more than once, as GLib's lookup in a
// GVariantDict ends up with. For a{sv} it is the variant itself.
func (v Value) Lookup(key string) (Value, bool, error) {
	if len(v.Type) < 4 || v.Type[:3] != "a{s" {
		return Value{}, false, fmt.Errorf("gvariant: %s is not a dictionary with string keys", v.Type)
	}
	entries, err := v.Elements()
	if err != nil {
		return Value{}, false, err
	}
	var found Value
	ok := false
	for _, e := range entries {
		kv, err := e.Members()
		if err != nil {
			return Value{}, false, err
		}
		k, err := kv[0].String()
		if err != nil {
			return Value{}, false, err
		}
		if k == key {
			found, ok = kv[1], true
		}
	}
	return found, ok, nil
}
