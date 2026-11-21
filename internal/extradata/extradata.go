// Package extradata replaces the download of an extra-data source of an
// app (rpak install/update/patch --extra-data=[NAME=]URL), an experimental
// feature: the app then installs a file its packagers did not pick, which
// is the user's call.
//
// flatpak downloads what the commit's xa.extra-data-sources says, which
// flatpak build-export makes from the [Extra Data] group of the metadata
// (collect_extra_data() in app/flatpak-builtins-build-export.c), and
// checks it against the checksum and size there. No configuration of
// flatpak changes that: the commit has to. So the upstream commit is
// reexported (see package forceruntime) with the source's uri, checksum
// and size rewritten (Rewrite); everything else, apply_extra included,
// stays upstream's.
//
// Only the source is replaced, for as long as upstream keeps it: the
// checksum upstream had when it was patched is kept (Replacement.Base),
// and once upstream's commit has another one the patch is dropped (Carry),
// upstream having moved on by itself.
package extradata

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/flatpark/rpak/internal/metadata"
)

// Option is rpak's option to replace an extra-data source; its value is
// [NAME=]URL and may be given again for other sources.
const Option = "--extra-data"

// Spec is one --extra-data value: NAME, the extra-data file name of the
// source to replace ("" to have it chosen, see Choose), and the URL to
// download it from instead.
type Spec struct {
	Name, URL string
}

// ParseSpec reads [NAME=]URL. An "=" before anything that could only be
// part of a URL (":" or "/") ends a NAME; a URL may have its own "=" in the
// query.
func ParseSpec(v string) (Spec, error) {
	var s Spec
	if name, rest, ok := strings.Cut(v, "="); ok && !strings.ContainsAny(name, ":/") {
		s.Name, s.URL = name, rest
		if !metadata.ValidName(name) {
			return s, fmt.Errorf("%s=%s: %q is not an extra-data file name", Option, v, name)
		}
	} else {
		s.URL = v
	}
	// flatpak_dir_pull_extra_data_to_bytes() only downloads http: and
	// https:.
	u, err := url.Parse(s.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return s, fmt.Errorf("%s=%s: want [NAME=]URL with an http or https URL", Option, v)
	}
	return s, nil
}

// ChoiceError is a spec that names no source, or that could replace more
// than one: the user is to say which, with NAME=URL.
type ChoiceError struct {
	Spec    Spec
	Reason  string
	Sources []metadata.Source
}

func (e *ChoiceError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s; give the source as NAME=URL, one of:", e.Reason)
	for _, s := range e.Sources {
		fmt.Fprintf(&b, "\n  %s  %s", s.Name, s.URI)
	}
	return b.String()
}

// Choose picks the source spec replaces among an app's sources (those of
// its architecture, as its metadata has them), in this order:
//
//  1. the source called spec.Name, if given;
//  2. the only source;
//  3. the source whose URL is clearly the closest to spec.URL: of those on
//     the same host, the one sharing the longest leading directories with
//     it, if no other shares as many (on github.com, the same owner/repo
//     and nothing further: releases of one project all look alike);
//  4. none: a ChoiceError lists the sources.
//
// A vendor's new release usually sits next to its old one, while the
// support files of an app (flatpark/prebuilt, deb.debian.org, a JRE...)
// do not, so 3 picks the app's own package when there is a choice at all.
// Nothing else is guessed (version numbers, which hosts are "support"):
// a wrong guess would replace the wrong file without a word. auto reports
// 2 or 3, for the caller to say which source it took.
func Choose(sources []metadata.Source, spec Spec) (src metadata.Source, auto bool, err error) {
	if spec.Name != "" {
		for _, s := range sources {
			if s.Name == spec.Name {
				return s, false, nil
			}
		}
		return src, false, &ChoiceError{Spec: spec, Reason: fmt.Sprintf("the app has no extra-data called %s", spec.Name), Sources: sources}
	}
	switch len(sources) {
	case 0:
		return src, false, &ChoiceError{Spec: spec, Reason: "the app has no extra-data rpak can replace"}
	case 1:
		return sources[0], true, nil
	}
	newURL, err := url.Parse(spec.URL)
	if err != nil {
		return src, false, err
	}
	best, bestCommon, tie := -1, -1, false
	for i, s := range sources {
		old, err := url.Parse(s.URI)
		if err != nil {
			continue
		}
		common, ok := affinity(newURL, old)
		switch {
		case !ok:
		case common > bestCommon:
			best, bestCommon, tie = i, common, false
		case common == bestCommon:
			tie = true
		}
	}
	if best < 0 || tie {
		return src, false, &ChoiceError{Spec: spec, Reason: fmt.Sprintf("cannot tell which extra-data %s replaces", spec.URL), Sources: sources}
	}
	return sources[best], true, nil
}

// affinity is how close two URLs are: ok if they are on the same host (and,
// on github.com, of the same owner/repo), and then how many leading
// directories of their paths they share.
func affinity(a, b *url.URL) (common int, ok bool) {
	if !strings.EqualFold(a.Host, b.Host) {
		return 0, false
	}
	da, db := dirs(a), dirs(b)
	if strings.EqualFold(a.Hostname(), "github.com") {
		if len(da) < 2 || len(db) < 2 || !strings.EqualFold(da[0], db[0]) || !strings.EqualFold(da[1], db[1]) {
			return 0, false
		}
		return 2, true
	}
	for common < len(da) && common < len(db) && da[common] == db[common] {
		common++
	}
	return common, true
}

// dirs are the directories of a URL's path, without the file name.
func dirs(u *url.URL) []string {
	var out []string
	for _, p := range strings.Split(u.EscapedPath(), "/") {
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) > 0 && !strings.HasSuffix(u.EscapedPath(), "/") {
		out = out[:len(out)-1]
	}
	return out
}

// File is a downloadable extra-data file as [Extra Data] describes it.
type File struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Replacement is a source replaced: its name, the file it is downloaded as
// now, and Base, upstream's file when it was replaced, which tells whether
// upstream has changed the source since.
type Replacement struct {
	Name string `json:"name"`
	File
	Base File `json:"base"`
}

// FileOf is the file a source describes.
func FileOf(s metadata.Source) File { return File{URL: s.URI, SHA256: s.Checksum, Size: s.Size} }

// Merge sets the replacements of names in add, keeping the others of old;
// the result is sorted by name.
func Merge(old, add []Replacement) []Replacement {
	byName := map[string]Replacement{}
	for _, r := range old {
		byName[r.Name] = r
	}
	for _, r := range add {
		byName[r.Name] = r
	}
	out := make([]Replacement, 0, len(byName))
	for _, r := range byName {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Carry decides what becomes of replacements when upstream's metadata is
// upstreamMeta (a new commit): a source upstream still has with the base
// checksum is kept, its base URL and size following upstream's (a mirror
// moved, the file is the same); a source upstream changed, or no longer
// has, is dropped: upstream caught up, or went elsewhere, by itself.
func Carry(reps []Replacement, upstreamMeta, ref string) (kept, dropped []Replacement) {
	sources, _ := metadata.Parse(upstreamMeta, ref)
	for _, r := range reps {
		found := false
		for _, s := range sources {
			if s.Name == r.Name && s.Checksum == r.Base.SHA256 {
				r.Base = FileOf(s)
				kept = append(kept, r)
				found = true
				break
			}
		}
		if !found {
			dropped = append(dropped, r)
		}
	}
	return kept, dropped
}

// Marks are what a reexported commit's body says of the replacements, for
// telling whether an export is still what the replacements want:
// NAME:SHA256, sorted.
func Marks(reps []Replacement) []string {
	out := make([]string, 0, len(reps))
	for _, r := range reps {
		out = append(out, r.Name+":"+r.SHA256)
	}
	sort.Strings(out)
	return out
}

// Rewrite makes the [Extra Data] entries of the replacements' sources
// download their new files: uri, checksum and size of each are replaced.
// Every other byte stays as it is, the file is rewritten line by line
// (installed-size too: what the new file unpacks to is not known before
// apply_extra runs, and flatpak only shows it). An entry that took its
// name from its uri gets a name key after its uri, so that the name, which
// apply_extra knows the file by, stays.
func Rewrite(text, ref string, reps []Replacement) (string, error) {
	entries, _ := metadata.ParseEntries(text, ref)
	type edit struct {
		rep   Replacement
		named bool
		seen  bool
	}
	bySuffix := map[string]*edit{}
	for _, r := range reps {
		found := false
		for _, e := range entries {
			if e.Name == r.Name {
				bySuffix[e.Suffix] = &edit{rep: r, named: e.Named}
				found = true
				break
			}
		}
		if !found {
			return "", fmt.Errorf("the metadata has no extra-data called %s", r.Name)
		}
	}
	lines := strings.SplitAfter(text, "\n")
	var out []string
	group := ""
	for _, line := range lines {
		out = append(out, line)
		body := strings.TrimRight(line, "\r\n")
		eol := line[len(body):]
		trimmed := strings.TrimLeft(body, " \t")
		if trimmed == "" || trimmed[0] == '#' {
			continue
		}
		if trimmed[0] == '[' {
			group = ""
			if end := strings.IndexByte(trimmed, ']'); end > 0 {
				group = trimmed[1:end]
			}
			continue
		}
		if group != "Extra Data" {
			continue
		}
		keyPart, value, ok := strings.Cut(body, "=")
		if !ok {
			continue
		}
		key := strings.Trim(keyPart, " \t")
		var field, suffix string
		for _, f := range []string{"uri", "checksum", "size"} {
			if s, ok := strings.CutPrefix(key, f); ok {
				if _, ok := bySuffix[s]; ok {
					field, suffix = f, s
				}
			}
		}
		if field == "" {
			continue
		}
		e := bySuffix[suffix]
		nv := map[string]string{"uri": escape(e.rep.URL), "checksum": e.rep.SHA256, "size": strconv.FormatInt(e.rep.Size, 10)}[field]
		lead := len(value) - len(strings.TrimLeft(value, " \t"))
		v := strings.TrimRight(value[lead:], " \t")
		out[len(out)-1] = keyPart + "=" + value[:lead] + nv + value[lead+len(v):] + eol
		if field == "uri" && !e.named && !e.seen {
			if eol == "" {
				// The last line had no end of line; the new one goes after.
				eol = "\n"
				out[len(out)-1] += eol
			}
			out = append(out, "name"+suffix+"="+escape(e.rep.Name)+eol)
			e.seen = true
		}
	}
	return strings.Join(out, ""), nil
}

// escape is g_key_file_set_string()'s escaping of a value: what
// metadata.KeyFile reads back as it was.
func escape(v string) string {
	var b strings.Builder
	for i, c := range v {
		switch {
		case c == ' ' && i == 0:
			b.WriteString(`\s`)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\t':
			b.WriteString(`\t`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\\':
			b.WriteString(`\\`)
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}
