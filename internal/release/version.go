package release

import (
	"strconv"
	"strings"
)

// Version is a version of rpak as "git describe --tags --always --dirty"
// gives it: a release tag vMAJOR.MINOR.PATCH, possibly followed by -N-gHASH
// (N commits after the tag) and -dirty (uncommitted changes).
type Version struct {
	Major, Minor, Patch int
	// Commits is N of -N-gHASH: the build was made that many commits
	// after the tag.
	Commits int
	Dirty   bool
}

// Dev reports whether v is a development build: not a release as tagged.
func (v Version) Dev() bool { return v.Commits > 0 || v.Dirty }

// Tag is the release tag v is, or was built after.
func (v Version) Tag() string {
	return "v" + strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor) + "." + strconv.Itoa(v.Patch)
}

// ParseTag parses a release tag: vMAJOR.MINOR.PATCH and nothing else.
func ParseTag(s string) (Version, bool) {
	rest, ok := strings.CutPrefix(s, "v")
	if !ok {
		return Version{}, false
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 3 {
		return Version{}, false
	}
	var n [3]int
	for i, p := range parts {
		if n[i], ok = number(p); !ok {
			return Version{}, false
		}
	}
	return Version{Major: n[0], Minor: n[1], Patch: n[2]}, true
}

// ParseBuild parses the version a binary was built as: a release tag,
// with -N-gHASH and -dirty as git describe adds them. It returns false for
// anything else ("dev", a bare commit hash), which is a development build
// that cannot be compared.
func ParseBuild(s string) (Version, bool) {
	rest, dirty := strings.CutSuffix(s, "-dirty")
	tag, describe, _ := strings.Cut(rest, "-")
	v, ok := ParseTag(tag)
	if !ok {
		return Version{}, false
	}
	v.Dirty = dirty
	if describe != "" {
		commits, hash, ok := strings.Cut(describe, "-g")
		if v.Commits, ok = number(commits); !ok || v.Commits == 0 || !isHex(hash) {
			return Version{}, false
		}
	}
	return v, true
}

// Compare returns -1, 0 or 1 as a is older than, the same as or newer than
// b. A build N commits after a tag is newer than the tag (and older than
// the next one); -dirty does not count.
func Compare(a, b Version) int {
	for _, d := range [][2]int{{a.Major, b.Major}, {a.Minor, b.Minor}, {a.Patch, b.Patch}, {a.Commits, b.Commits}} {
		switch {
		case d[0] < d[1]:
			return -1
		case d[0] > d[1]:
			return 1
		}
	}
	return 0
}

// number parses a part of a version: decimal digits, no leading zero.
func number(s string) (int, bool) {
	if s == "" || len(s) > 9 || (len(s) > 1 && s[0] == '0') {
		return 0, false
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}
