package forceruntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Methods of forcing, see the package comment.
const (
	MethodPatch    = "patch"
	MethodReexport = "reexport"
)

// RemoteName is the remote of the local repository reexported apps are
// installed from. It is added when the first reexported app needs it and
// deleted with the repository when the last one is reset, so users who do
// not use this never see it.
const RemoteName = "rpak-local"

// Record is an app whose runtime is forced.
type Record struct {
	// Ref is the app's full ref, app/ID/ARCH/BRANCH: the key in the file.
	Ref string `json:"-"`
	// Target is the --force-runtime value as given: latest, a branch or a
	// runtime pref.
	Target string `json:"target"`
	// Remote is the upstream remote the app comes from.
	Remote string `json:"remote"`
	Method string `json:"method"`
	// Original is the runtime upstream's commit names, last seen.
	Original string `json:"original_runtime"`
	// Runtime is the runtime the app was last made to run on.
	Runtime string `json:"runtime"`
	// Upstream is the upstream commit last reexported (reexport only).
	Upstream string `json:"upstream_commit,omitempty"`
}

// ID is the app's id.
func (r Record) ID() string {
	parts := strings.Split(r.Ref, "/")
	if len(parts) == 4 {
		return parts[1]
	}
	return r.Ref
}

// The patches file holds what rpak changed about installed apps, by app:
//
//	{"version": 1, "apps": {"app/ID/ARCH/BRANCH": {"force-runtime": {...}}}}
//
// force-runtime is the only kind of patch so far. Kinds rpak does not know
// are kept as they are when the file is rewritten.
type file struct {
	Version int                                   `json:"version"`
	Apps    map[string]map[string]json.RawMessage `json:"apps"`
}

const (
	fileVersion = 1
	// Kind is the name of this patch in the patches file.
	Kind = "force-runtime"
)

// Dir is where rpak keeps what it needs for patching in an installation:
// <installation>/rpak, which moves with FLATPAK_USER_DIR.
func Dir(installation string) string { return filepath.Join(installation, "rpak") }

// RepoPath is the local repository of the reexported apps.
func RepoPath(installation string) string { return filepath.Join(Dir(installation), "local-repo") }

// RecordsPath is the patches file.
func RecordsPath(installation string) string {
	return filepath.Join(Dir(installation), "patches.json")
}

// HasRecords reports whether the patches file exists, without reading it:
// the check that commands without --force-runtime make, so that they do
// nothing else when nothing is patched.
func HasRecords(installation string) bool {
	_, err := os.Lstat(RecordsPath(installation))
	return err == nil
}

func readFile(installation string) (file, error) {
	data, err := os.ReadFile(RecordsPath(installation))
	if errors.Is(err, fs.ErrNotExist) {
		return file{Version: fileVersion}, nil
	}
	if err != nil {
		return file{}, err
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return file{}, fmt.Errorf("%s: %w", RecordsPath(installation), err)
	}
	if f.Version != fileVersion {
		return file{}, fmt.Errorf("%s: unknown version %d", RecordsPath(installation), f.Version)
	}
	return f, nil
}

// Load reads the force-runtime patches, sorted by ref; a missing file has
// none.
func Load(installation string) ([]Record, error) {
	f, err := readFile(installation)
	if err != nil {
		return nil, err
	}
	var out []Record
	for ref, patches := range f.Apps {
		raw, ok := patches[Kind]
		if !ok {
			continue
		}
		var r Record
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, fmt.Errorf("%s: %s: %w", RecordsPath(installation), ref, err)
		}
		r.Ref = ref
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, nil
}

// Save makes records the force-runtime patches of the file, keeping other
// kinds of patches, through a temporary file renamed over the old one.
// A file left without patches is removed: no file is the cheap sign that
// nothing is patched.
func Save(installation string, records []Record) error {
	path := RecordsPath(installation)
	f, err := readFile(installation)
	if err != nil {
		// Unreadable: what is in it cannot be kept anyway.
		f = file{Version: fileVersion}
	}
	if f.Apps == nil {
		f.Apps = map[string]map[string]json.RawMessage{}
	}
	for _, patches := range f.Apps {
		delete(patches, Kind)
	}
	for _, r := range records {
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if f.Apps[r.Ref] == nil {
			f.Apps[r.Ref] = map[string]json.RawMessage{}
		}
		f.Apps[r.Ref][Kind] = raw
	}
	for ref, patches := range f.Apps {
		if len(patches) == 0 {
			delete(f.Apps, ref)
		}
	}
	if len(f.Apps) == 0 {
		err := os.Remove(path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	data, err := json.MarshalIndent(f, "", "  ") // map keys sorted
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return WriteFileAtomic(path, append(data, '\n'), 0o644)
}

// Set replaces the record of r.Ref, or adds it.
func Set(records []Record, r Record) []Record {
	for i := range records {
		if records[i].Ref == r.Ref {
			records[i] = r
			return records
		}
	}
	return append(records, r)
}

// Remove drops the record of ref.
func Remove(records []Record, ref string) []Record {
	out := records[:0:0]
	for _, r := range records {
		if r.Ref != ref {
			out = append(out, r)
		}
	}
	return out
}

// Find returns the record of ref.
func Find(records []Record, ref string) (Record, bool) {
	for _, r := range records {
		if r.Ref == ref {
			return r, true
		}
	}
	return Record{}, false
}

// WriteFileAtomic writes data to a temporary file next to path and renames
// it over path, so that path is never seen half-written and, above all, a
// file that is a hard link (a deployment's files are links to the objects
// of the repository) is replaced instead of written through.
func WriteFileAtomic(path string, data []byte, mode fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".rpak-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() {
		if name != "" {
			os.Remove(name)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	name = ""
	return nil
}
