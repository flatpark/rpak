// Package forceruntime makes apps run on a newer branch of their runtime
// than the one they were built for (rpak install/update/patch
// --force-runtime), an experimental feature: the app has not been tested
// on that runtime.
//
// flatpak run takes the runtime from [Application] runtime= of the
// deployment's metadata file (flatpak_run_app() in common/flatpak-run.c)
// and does not check it against anything; only deploying compares the
// checked-out metadata with the commit's xa.metadata
// (validate_commit_metadata() in common/flatpak-dir.c). So an app is forced
// in one of two ways:
//
//   - patch: after flatpak has deployed the app, the deployment's metadata
//     and the runtime in its deploy file are rewritten (see Patch);
//   - reexport: for an app with extra-data, whose apply_extra runs in the
//     runtime the metadata names while deploying (apply_extra_data()), the
//     upstream commit is checked out, its metadata rewritten, and the
//     result exported into a local repository that flatpak installs from.
//
// The forcing is remembered (Record) so that later updates keep it.
package forceruntime

import (
	"cmp"
	"fmt"
	"regexp"
	"strings"
)

// Latest is the target that picks the newest branch of the same form.
const Latest = "latest"

// Pref is a runtime pref, ID/ARCH/BRANCH, as [Application] runtime= has it.
type Pref struct {
	ID, Arch, Branch string
}

func (p Pref) String() string { return p.ID + "/" + p.Arch + "/" + p.Branch }

// Ref is the full ref of the runtime.
func (p Pref) Ref() string { return "runtime/" + p.String() }

// ParsePref reads ID/ARCH/BRANCH, also with a leading "runtime/".
func ParsePref(s string) (Pref, error) {
	s = strings.TrimPrefix(s, "runtime/")
	parts := strings.Split(s, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return Pref{}, fmt.Errorf("%q is not a runtime ID/ARCH/BRANCH", s)
	}
	return Pref{parts[0], parts[1], parts[2]}, nil
}

// Branch forms whose versions can be compared. A branch is only ever
// compared with, and replaced by, a branch of the same form: KDE's
// 5.15-24.08 and 6.9 are different runtimes, not versions of one.
var forms = []*regexp.Regexp{
	regexp.MustCompile(`^\d+\.\d+$`),       // freedesktop 25.08, KDE 6.9
	regexp.MustCompile(`^\d+$`),            // GNOME 49
	regexp.MustCompile(`^5\.15-\d+\.\d+$`), // KDE 5.15-24.08
}

// form is the index of the branch's form in forms, -1 for none.
func form(branch string) int {
	for i, re := range forms {
		if re.MatchString(branch) {
			return i
		}
	}
	return -1
}

var digits = regexp.MustCompile(`\d+`)

// compareVersions compares two branches of the same form number by
// number: 25.08 < 25.10 < 26.08, 9 < 49. Numbers are compared as digit
// strings, so that no length overflows.
func compareVersions(a, b string) int {
	na, nb := digits.FindAllString(a, -1), digits.FindAllString(b, -1)
	for i := 0; i < len(na) && i < len(nb); i++ {
		x, y := strings.TrimLeft(na[i], "0"), strings.TrimLeft(nb[i], "0")
		if c := cmp.Compare(len(x), len(y)); c != 0 {
			return c
		}
		if c := strings.Compare(x, y); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(na), len(nb))
}

// LatestBranch picks the newest of candidates that has the form of old,
// old itself included. ok is false if old has no known form.
func LatestBranch(old string, candidates []string) (branch string, ok bool) {
	f := form(old)
	if f < 0 {
		return "", false
	}
	best := old
	for _, c := range candidates {
		if form(c) == f && compareVersions(c, best) > 0 {
			best = c
		}
	}
	return best, true
}

// Resolution is what an app's runtime is forced to.
type Resolution struct {
	Original Pref
	// Runtime is the runtime to run on; it is Original when Forced is
	// false.
	Runtime Pref
	// Forced is false when the target is not newer than the original:
	// upstream is already there (or past it), nothing is forced.
	Forced bool
}

// Resolve works out the runtime for an app built for original, given a
// target as the user wrote it: Latest, a branch, or a runtime pref (which
// may name another runtime; that is the user's call). branches lists the
// branches of the original runtime known to exist, for Latest. Only
// upgrades are made: a target that is not newer than the original, as far
// as the branches can be compared, resolves to the original. Branches of
// different forms cannot be compared; an explicit target of another form
// is taken as asked.
func Resolve(original Pref, target string, branches []string) (Resolution, error) {
	r := Resolution{Original: original, Runtime: original}
	var want Pref
	switch {
	case target == "" || target == Latest:
		b, ok := LatestBranch(original.Branch, branches)
		if !ok {
			return r, fmt.Errorf("cannot tell which branch of %s is the latest after %s; give the branch instead of latest", original.ID, original.Branch)
		}
		want = Pref{original.ID, original.Arch, b}
	case strings.Contains(target, "/"):
		// ID//BRANCH leaves the architecture to the app's.
		parts := strings.Split(strings.TrimPrefix(target, "runtime/"), "/")
		if len(parts) == 3 && parts[1] == "" {
			parts[1] = original.Arch
		}
		p, err := ParsePref(strings.Join(parts, "/"))
		if err != nil {
			return r, err
		}
		if err := ValidBranch(p.Branch); err != nil {
			return r, err
		}
		if p.Arch != original.Arch {
			return r, fmt.Errorf("%s is for %s, the app for %s", p, p.Arch, original.Arch)
		}
		want = p
	default:
		if err := ValidBranch(target); err != nil {
			return r, err
		}
		want = Pref{original.ID, original.Arch, target}
	}
	if want == original {
		return r, nil
	}
	if want.ID == original.ID {
		f := form(original.Branch)
		if f >= 0 && form(want.Branch) == f && compareVersions(want.Branch, original.Branch) <= 0 {
			return r, nil
		}
	}
	r.Runtime, r.Forced = want, true
	return r, nil
}

// ValidBranch is the gist of flatpak_is_valid_branch(): letters, digits,
// "_", "-" and ".", not starting with "-" or ".".
func ValidBranch(b string) error {
	if b == "" {
		return fmt.Errorf("empty branch")
	}
	for i, c := range b {
		ok := c == '_' || c == '-' || c == '.' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if !ok || (i == 0 && (c == '-' || c == '.')) {
			return fmt.Errorf("%q is not a valid branch", b)
		}
	}
	return nil
}

// ValidTarget checks a --force-runtime value as far as that can be done
// without knowing the app.
func ValidTarget(target string) error {
	switch {
	case target == "" || target == Latest:
		return nil
	case strings.Contains(target, "/"):
		s := strings.TrimPrefix(target, "runtime/")
		parts := strings.Split(s, "/")
		if len(parts) != 3 || parts[0] == "" || parts[2] == "" {
			return fmt.Errorf("--force-runtime=%s: want latest, a BRANCH or ID/ARCH/BRANCH", target)
		}
		return ValidBranch(parts[2])
	}
	return ValidBranch(target)
}
