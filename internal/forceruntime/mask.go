package forceruntime

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/flatpark/rpak/internal/metadata"
)

// Masked reports whether "flatpak mask" keeps ref from being updated in an
// installation: flatpak update skips a masked ref
// (flatpak_dir_needs_update_for_commit_and_subpaths()), so rpak, which
// reexports, reinstalls and records on its own when upstream moves, must
// skip it too, or its records would say what flatpak did not do. The
// patterns are the [core] xa.masked of the installation's repo/config,
// separated by ";" (flatpak_dir_get_mask_regexp()).
func Masked(installation, ref string) bool {
	text, err := os.ReadFile(filepath.Join(installation, "repo", "config"))
	if err != nil {
		return false
	}
	for _, g := range metadata.KeyFile(string(text)) {
		if g.Name == "core" {
			return MaskMatches(g.Values["xa.masked"], ref)
		}
	}
	return false
}

// MaskMatches reports whether a ref matches one of the ";"-separated mask
// patterns; patterns flatpak would reject match nothing.
func MaskMatches(patterns, ref string) bool {
	for _, p := range strings.Split(patterns, ";") {
		if p == "" {
			continue
		}
		if re, ok := globRegexp(p); ok && re.MatchString(ref) {
			return true
		}
	}
	return false
}

// globRegexp is flatpak_filter_glob_to_regexp() (common/flatpak-utils.c)
// for masks: [app/|runtime/]ID[/ARCH[/BRANCH]], "*" any run of the
// characters of a ref part, a missing or empty part anything; anchored at
// both ends.
func globRegexp(glob string) (*regexp.Regexp, bool) {
	const part = `[.\-_a-zA-Z0-9]*`
	var b strings.Builder
	b.WriteString("^")
	switch {
	case strings.HasPrefix(glob, "app/"):
		glob = glob[len("app/"):]
		b.WriteString("app/")
	case strings.HasPrefix(glob, "runtime/"):
		glob = glob[len("runtime/"):]
		b.WriteString("runtime/")
	default:
		b.WriteString("(app|runtime)/")
	}
	if glob == "" {
		return nil, false
	}
	parts, empty := 1, true
	for _, c := range glob {
		switch {
		case c == '/':
			if empty {
				b.WriteString(part)
			}
			empty = true
			parts++
			b.WriteString("/")
			if parts > 3 {
				return nil, false
			}
		case c == '*':
			empty = false
			b.WriteString(part)
		case c == '.':
			empty = false
			b.WriteString(`\.`)
		case c == '-' || c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
			empty = false
			b.WriteRune(c)
		default:
			return nil, false
		}
	}
	// Like flatpak: a trailing "/" leaves its part empty, which matches
	// only an empty part; a missing part matches anything.
	for ; parts < 3; parts++ {
		b.WriteString("/" + part)
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	return re, err == nil
}
