// Package metadata extracts the [Extra Data] sources from the metadata that
// `flatpak remote-info --show-metadata` prints.
package metadata

import (
	"bufio"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Source is one extra-data file of a ref.
type Source struct {
	Ref      string
	Name     string
	Checksum string // lowercase hex SHA-256
	Size     int64
	URI      string
}

// Skipped is an [Extra Data] entry that rpak leaves to flatpak.
type Skipped struct {
	Key    string // the uri* key of the entry
	Reason string
}

// Parse returns the sources rpak can prefetch and the entries it skips.
//
// The rules follow collect_extra_data() in flatpak's
// app/flatpak-builtins-build-export.c, which turns this group into the
// commit's xa.extra-data-sources: every key starting with "uri" is an entry,
// and the rest of the key is the suffix of its name/checksum/size keys.
// Anything we cannot reproduce exactly is skipped: a wrong name or path only
// means flatpak downloads the file itself.
func Parse(text, ref string) ([]Source, []Skipped) {
	values, order := extraDataGroup(text)
	var sources []Source
	var skipped []Skipped
	for _, key := range order {
		if !strings.HasPrefix(key, "uri") {
			continue
		}
		suffix := key[len("uri"):]
		source, err := parseSource(values, suffix)
		if err != nil {
			skipped = append(skipped, Skipped{Key: key, Reason: err.Error()})
			continue
		}
		source.Ref = ref
		sources = append(sources, source)
	}
	return sources, skipped
}

func parseSource(values map[string]string, suffix string) (Source, error) {
	uri := values["uri"+suffix]
	// flatpak_dir_pull_extra_data_to_bytes() only downloads http: and https:.
	if !strings.HasPrefix(uri, "http:") && !strings.HasPrefix(uri, "https:") {
		return Source{}, fmt.Errorf("unsupported uri %q", uri)
	}
	u, err := url.Parse(uri)
	if err != nil || u.Host == "" {
		return Source{}, fmt.Errorf("invalid uri %q", uri)
	}

	checksum := values["checksum"+suffix]
	if !validChecksum(checksum) {
		return Source{}, fmt.Errorf("invalid checksum %q", checksum)
	}

	size, err := strconv.ParseInt(values["size"+suffix], 10, 64)
	if err != nil || size <= 0 {
		return Source{}, fmt.Errorf("invalid size %q", values["size"+suffix])
	}

	name, ok := values["name"+suffix]
	if !ok {
		name, err = basename(u)
		if err != nil {
			return Source{}, err
		}
	}
	// The same checks flatpak_dir_pull_extra_data_to_bytes() applies before
	// building <user dir>/extra-data/<sha256>/<name>.
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") || strings.ContainsRune(name, 0) {
		return Source{}, fmt.Errorf("invalid name %q", name)
	}
	return Source{Name: name, Checksum: checksum, Size: size, URI: uri}, nil
}

// basename mirrors g_file_get_basename (g_file_new_for_uri (uri)) for the
// unambiguous cases: the last segment of the URL path, percent-decoded.
// GIO's handling of query strings, fragments and trailing slashes is not
// something to guess at, so those are refused.
func basename(u *url.URL) (string, error) {
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", fmt.Errorf("no name and uri has a query or fragment")
	}
	escaped := u.EscapedPath()
	if escaped == "" || strings.HasSuffix(escaped, "/") {
		return "", fmt.Errorf("no name and uri has no basename")
	}
	name, err := url.PathUnescape(escaped[strings.LastIndex(escaped, "/")+1:])
	if err != nil {
		return "", fmt.Errorf("no name and uri basename is not decodable")
	}
	return name, nil
}

// extraDataGroup reads the [Extra Data] group with GKeyFile semantics: the
// last occurrence of a key wins, groups may repeat, values are unescaped.
// Keys are returned in first-seen order.
func extraDataGroup(text string) (map[string]string, []string) {
	values := make(map[string]string)
	var order []string
	inGroup := false
	scanner := bufio.NewScanner(strings.NewReader(text))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimLeft(scanner.Text(), " \t")
		if line == "" || line[0] == '#' {
			continue
		}
		if line[0] == '[' {
			end := strings.IndexByte(line, ']')
			inGroup = end > 0 && line[1:end] == "Extra Data"
			continue
		}
		if !inGroup {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimRight(key, " \t")
		if _, seen := values[key]; !seen {
			order = append(order, key)
		}
		values[key] = unescape(strings.TrimLeft(value, " \t"))
	}
	return values, order
}

// unescape handles the escapes g_key_file_get_string() understands.
func unescape(value string) string {
	if !strings.Contains(value, `\`) {
		return value
	}
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] != '\\' || i+1 == len(value) {
			b.WriteByte(value[i])
			continue
		}
		i++
		switch value[i] {
		case 's':
			b.WriteByte(' ')
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		default:
			b.WriteByte(value[i])
		}
	}
	return b.String()
}

// validChecksum matches ostree_validate_checksum_string(): 64 lowercase hex.
func validChecksum(value string) bool {
	if len(value) != 64 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
