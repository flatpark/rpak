// Package summary reads what flatpak reads from a remote's summary to find
// a ref's commit and metadata, as flatpak 1.18 does it
// (_flatpak_dir_get_remote_state(), flatpak_remote_state_lookup_cache()
// and flatpak_remote_state_lookup_ref() in common/flatpak-dir.c, types in
// data/flatpak-variants.gv):
//
//   - A remote with summary.idx, (a{s(ayaaya{sv})}a{sv}), has a
//     subsummary per architecture, named by the architecture, or
//     "SUBSET-ARCH" for the subsets a remote can be limited to (xa.subset).
//     It is summaries/<sha256>.gz, gzip-compressed, where the digest is
//     that of the uncompressed subsummary and is checked.
//   - A remote without summary.idx has a single summary.
//   - Both have the type of ostree's summary, (a(s(taya{sv}))a{sv}): refs,
//     sorted, with the commit checksum and the commit's metadata, then the
//     summary's own metadata. Where its xa.summary-version is 1, a ref's
//     metadata (the app's metadata file) is the third member of xa.data,
//     (tts), in the ref's own metadata; where it is 0 or missing, it is in
//     xa.cache, a variant of a variant of a{s(tts)} by ref.
//
// Signatures are not checked: this is for diagnosing, and it compares
// what the remote serves with what developers' servers serve.
package summary

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/flatpark/rpak/internal/gvariant"
)

// IndexType and Type are the GVariant types of summary.idx and of a
// (sub)summary.
const (
	IndexType = "(a{s(ayaaya{sv})}a{sv})"
	Type      = "(a(s(taya{sv}))a{sv})"
)

// Subsummaries reads a summary.idx: the hex SHA-256 of every subsummary,
// by name ("x86_64", "flathub-x86_64", ...).
func Subsummaries(index []byte) (map[string]string, error) {
	top, err := gvariant.Value{Type: IndexType, Data: index}.Members()
	if err != nil {
		return nil, fmt.Errorf("summary.idx: %w", err)
	}
	entries, err := top[0].Elements()
	if err != nil {
		return nil, fmt.Errorf("summary.idx: %w", err)
	}
	out := map[string]string{}
	for _, e := range entries {
		kv, err := e.Members()
		if err != nil {
			return nil, fmt.Errorf("summary.idx: %w", err)
		}
		name, err := kv[0].String()
		if err != nil {
			return nil, fmt.Errorf("summary.idx: %w", err)
		}
		sub, err := kv[1].Members()
		if err != nil {
			return nil, fmt.Errorf("summary.idx: %w", err)
		}
		sum, err := sub[0].Bytes()
		if err != nil || len(sum) != sha256.Size {
			// flatpak skips these too ("Invalid checksum for digested
			// summary").
			continue
		}
		out[name] = hex.EncodeToString(sum)
	}
	return out, nil
}

// SubsummaryName is the subsummary flatpak uses for an architecture,
// given the remote's xa.subset ("" for none).
func SubsummaryName(arch, subset string) string {
	if subset == "" {
		return arch
	}
	return subset + "-" + arch
}

// Verify checks that an uncompressed subsummary has the digest the index
// gives for it.
func Verify(data []byte, digest string) error {
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != digest {
		return fmt.Errorf("subsummary checksum is %s, not %s", got, digest)
	}
	return nil
}

// Ref is a ref of a summary.
type Ref struct {
	Name     string // app/ID/ARCH/BRANCH
	Commit   string // hex
	Metadata string // the metadata file of the ref's commit, "" if missing
}

// Find returns the refs of a summary whose name starts with prefix
// ("app/org.x.Y/x86_64/" for every branch, or a full ref), in summary
// order.
func Find(data []byte, prefix string) ([]Ref, error) {
	top, err := gvariant.Value{Type: Type, Data: data}.Members()
	if err != nil {
		return nil, fmt.Errorf("summary: %w", err)
	}
	refs, meta := top[0], top[1]
	version := uint32(0)
	if v, ok, err := meta.Lookup("xa.summary-version"); err != nil {
		return nil, fmt.Errorf("summary: %w", err)
	} else if ok {
		if inner, err := v.Variant(); err == nil {
			version, _ = inner.Uint32()
		}
	}
	if version > 1 {
		return nil, fmt.Errorf("summary: unsupported summary version %d", version)
	}

	entries, err := refs.Elements()
	if err != nil {
		return nil, fmt.Errorf("summary: %w", err)
	}
	var out []Ref
	for _, e := range entries {
		kv, err := e.Members()
		if err != nil {
			return nil, fmt.Errorf("summary: %w", err)
		}
		name, err := kv[0].String()
		if err != nil {
			return nil, fmt.Errorf("summary: %w", err)
		}
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		info, err := kv[1].Members() // (taya{sv})
		if err != nil {
			return nil, fmt.Errorf("summary: %s: %w", name, err)
		}
		commit, err := info[1].Bytes()
		if err != nil {
			return nil, fmt.Errorf("summary: %s: %w", name, err)
		}
		r := Ref{Name: name, Commit: hex.EncodeToString(commit)}
		if version == 1 {
			r.Metadata, err = xaData(info[2])
		} else {
			r.Metadata, err = xaCache(meta, name)
		}
		if err != nil {
			return nil, fmt.Errorf("summary: %s: %w", name, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// xaData is the metadata in a ref's xa.data, (tts).
func xaData(commitMeta gvariant.Value) (string, error) {
	v, ok, err := commitMeta.Lookup("xa.data")
	if err != nil || !ok {
		return "", err
	}
	data, err := v.Variant()
	if err != nil {
		return "", err
	}
	return cacheData(data)
}

// xaCache is the metadata of a ref in the summary's xa.cache.
func xaCache(meta gvariant.Value, ref string) (string, error) {
	v, ok, err := meta.Lookup("xa.cache")
	if err != nil || !ok {
		return "", err
	}
	// "For stupid historical reasons the xa.cache is double-wrapped in a
	// variant": the dictionary's v holds a v holding the a{s(tts)}.
	for v.Type == "v" {
		if v, err = v.Variant(); err != nil {
			return "", err
		}
	}
	if v.Type != "a{s(tts)}" {
		return "", fmt.Errorf("xa.cache is %s", v.Type)
	}
	data, ok, err := v.Lookup(ref)
	if err != nil || !ok {
		return "", err
	}
	return cacheData(data)
}

// cacheData is the metadata of a (tts): installed size, download size,
// metadata.
func cacheData(v gvariant.Value) (string, error) {
	if v.Type != "(tts)" {
		return "", fmt.Errorf("cache data is %s", v.Type)
	}
	m, err := v.Members()
	if err != nil {
		return "", err
	}
	return m[2].String()
}
