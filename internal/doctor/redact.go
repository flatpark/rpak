package doctor

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// Redactor hides what identifies the user in doctor's output, which is
// meant to be pasted into a public issue: home directories become "~"
// (another user's, under sudo, "~<user>"), user names "<user>", and
// passwords in URLs "***". The host name is never collected at all.
//
// Only collected data goes through it (Sprintf redacts its arguments, not
// its format): a user called "user" must not turn doctor's own "User
// installation" into "<user> installation".
type Redactor struct {
	homes []homeName // longest first, so that a home inside another wins
	users []string
}

type homeName struct{ path, as string }

// NewRedactor hides home as "~", the other homes (path → user name) as
// "~<user>", and the user names. "root" is not hidden: it identifies no
// one, and replacing it would mangle paths and words alike.
func NewRedactor(home string, others map[string]string, users []string) Redactor {
	var r Redactor
	add := func(path, as string) {
		path = strings.TrimRight(path, "/")
		if path == "" {
			return
		}
		for _, h := range r.homes {
			if h.path == path {
				return
			}
		}
		r.homes = append(r.homes, homeName{path, as})
	}
	add(home, "~")
	for path := range others {
		add(path, "~<user>")
	}
	sort.SliceStable(r.homes, func(i, j int) bool { return len(r.homes[i].path) > len(r.homes[j].path) })
	for _, u := range users {
		if u != "" && u != "root" && !contains(r.users, u) {
			r.users = append(r.users, u)
		}
	}
	return r
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// nameChar is a character that continues a path component or a word; a
// home or user name only counts where it is not part of a longer one.
func nameChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.'
}

// replaceBounded replaces old in s wherever it is not preceded by a name
// character or followed by one for which after returns false.
func replaceBounded(s, old, new string, after func(byte) bool) string {
	if old == "" || !strings.Contains(s, old) {
		return s
	}
	var b strings.Builder
	for {
		i := strings.Index(s, old)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		end := i + len(old)
		if (i == 0 || !nameChar(s[i-1])) && (end == len(s) || after(s[end])) {
			b.WriteString(s[:i])
			b.WriteString(new)
		} else {
			b.WriteString(s[:end])
		}
		s = s[end:]
	}
}

// userinfo is the user:password@ of a URL anywhere in a text.
var userinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/@\s]+@`)

// String redacts a piece of collected text.
func (r Redactor) String(s string) string {
	s = userinfo.ReplaceAllString(s, "${1}***@")
	for _, h := range r.homes {
		s = replaceBounded(s, h.path, h.as, func(c byte) bool { return !nameChar(c) })
	}
	for _, u := range r.users {
		s = replaceBounded(s, u, "<user>", func(c byte) bool { return !nameChar(c) })
	}
	return s
}

// Sprintf is fmt.Sprintf with every argument redacted. Errors and other
// values are formatted first, so that their paths are redacted too.
func (r Redactor) Sprintf(format string, a ...any) string {
	args := make([]any, len(a))
	for i, v := range a {
		switch v := v.(type) {
		case string:
			args[i] = r.String(v)
		case error:
			args[i] = r.String(v.Error())
		case fmt.Stringer:
			args[i] = r.String(v.String())
		default:
			args[i] = v
		}
	}
	return fmt.Sprintf(format, args...)
}

// RedactURL hides the user name and password of a URL as "***", also in
// the scheme-less form proxy variables may take ("u:p@host:3128").
func RedactURL(s string) string {
	raw := s
	if !strings.Contains(s, "://") {
		raw = "http://" + s
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		// Not a URL we can take apart: drop everything before the last @.
		if i := strings.LastIndexByte(s, '@'); i >= 0 {
			return "***" + s[i:]
		}
		return s
	}
	if u.User == nil {
		return s
	}
	prefix := ""
	if i := strings.Index(s, "://"); i >= 0 {
		prefix = s[:i+3]
	}
	rest := s[len(prefix):]
	at := strings.LastIndexByte(rest[:hostEnd(rest)], '@')
	if at < 0 {
		return s
	}
	return prefix + "***" + rest[at:]
}

// hostEnd is where the authority of a scheme-less URL ends.
func hostEnd(s string) int {
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		return i
	}
	return len(s)
}
