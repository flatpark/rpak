package forceruntime

import (
	"fmt"
	"strings"

	"github.com/flatpark/rpak/internal/metadata"
)

// Metadata groups and keys rewritten, as flatpak names them
// (common/flatpak-metadata-private.h).
const (
	groupApplication = "Application"
	groupExtension   = "Extension " // FLATPAK_METADATA_GROUP_PREFIX_EXTENSION
	groupExtraData   = "Extra Data"
)

// RuntimeOf is the [Application] runtime= of an app's metadata.
func RuntimeOf(text string) (Pref, error) {
	for _, g := range metadata.KeyFile(text) {
		if g.Name == groupApplication {
			if v, ok := g.Values["runtime"]; ok {
				return ParsePref(v)
			}
		}
	}
	return Pref{}, fmt.Errorf("the metadata names no runtime: not an app")
}

// HasExtraData reports whether the metadata has an [Extra Data] group: the
// app runs apply_extra when it is deployed, in its runtime.
func HasExtraData(text string) bool {
	for _, g := range metadata.KeyFile(text) {
		if g.Name == groupExtraData {
			return true
		}
	}
	return false
}

// RewriteMetadata makes an app's metadata name the runtime to instead of
// from:
//
//   - [Application] runtime= becomes to;
//   - [Application] sdk= gets to's branch if it had from's, or uninstall
//     --unused would keep the old Sdk for it (the sdk is a dependency,
//     flatpak_dir_find_remote_related() / find_used_refs());
//   - in every [Extension ...] group, version=from's branch and the items of
//     versions= that are from's branch become to's branch: those are the
//     runtime's extensions (GL, codecs...), versioned with it.
//
// Every other byte stays as it is: the file is rewritten line by line, so
// comments, order, unknown keys, localized keys and line endings are kept.
// A runtime= that is not from is an error: the metadata is not what the
// caller thinks it is.
func RewriteMetadata(text string, from, to Pref) (string, error) {
	lines := strings.SplitAfter(text, "\n")
	group := ""
	sawRuntime := false
	for i, line := range lines {
		body := strings.TrimRight(line, "\r\n")
		eol := line[len(body):]
		trimmed := strings.TrimLeft(body, " \t")
		if trimmed == "" || trimmed[0] == '#' {
			continue
		}
		if trimmed[0] == '[' {
			if end := strings.IndexByte(trimmed, ']'); end > 0 {
				group = trimmed[1:end]
			} else {
				group = ""
			}
			continue
		}
		keyPart, value, ok := strings.Cut(body, "=")
		if !ok {
			continue
		}
		key := strings.Trim(keyPart, " \t")
		// The value as GKeyFile reads it: leading blanks dropped. Only the
		// value proper is replaced, whatever surrounds it stays.
		lead := len(value) - len(strings.TrimLeft(value, " \t"))
		v := strings.TrimRight(value[lead:], " \t")
		trail := value[lead+len(v):]
		replace := func(nv string) {
			lines[i] = keyPart + "=" + value[:lead] + nv + trail + eol
		}
		switch {
		case group == groupApplication && key == "runtime":
			p, err := ParsePref(v)
			if err != nil || p != from {
				return "", fmt.Errorf("the metadata has runtime=%s, not %s", v, from)
			}
			sawRuntime = true
			replace(to.String())
		case group == groupApplication && key == "sdk":
			if p, err := ParsePref(v); err == nil && p.Branch == from.Branch && p.Branch != to.Branch {
				p.Branch = to.Branch
				replace(p.String())
			}
		case strings.HasPrefix(group, groupExtension) && key == "version":
			if v == from.Branch {
				replace(to.Branch)
			}
		case strings.HasPrefix(group, groupExtension) && key == "versions":
			items := strings.Split(v, ";")
			changed := false
			for j, item := range items {
				if item == from.Branch {
					items[j] = to.Branch
					changed = true
				}
			}
			if changed {
				replace(strings.Join(items, ";"))
			}
		}
	}
	if !sawRuntime {
		return "", fmt.Errorf("the metadata names no runtime in [%s]", groupApplication)
	}
	return strings.Join(lines, ""), nil
}
