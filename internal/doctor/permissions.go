package doctor

import (
	"fmt"
	"sort"
	"strings"
)

// PermissionStore makes `flatpak permission-show ID` fit for a public
// issue. Its rows are TABLE, OBJECT, APP, PERMISSIONS and DATA, tab
// separated. The documents table has a row per file shared with the app
// through the document portal, and its data is the file's path: only the
// number of files is kept, by permissions, without paths or document ids.
// In other tables (screenshot, devices, notifications, ...) the data of
// any table, known or not, may hold paths too, so data that is a byte
// string or long is left out; the permissions are kept.
func PermissionStore(out string) string {
	var lines []string
	docs := map[string]bool{}
	byPerms := map[string]map[string]bool{}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if f[0] == "documents" {
			id, perms := "", ""
			if len(f) > 1 {
				id = f[1]
			}
			if len(f) > 3 {
				perms = f[3]
			}
			docs[id] = true
			if byPerms[perms] == nil {
				byPerms[perms] = map[string]bool{}
			}
			byPerms[perms][id] = true
			continue
		}
		if len(f) > 4 && hidesData(f[4]) {
			f[4] = "(data omitted)"
			f = f[:5]
		}
		lines = append(lines, strings.Join(f, "\t"))
	}
	if len(docs) > 0 {
		var kinds []string
		for perms, ids := range byPerms {
			if perms == "" {
				perms = "no permissions"
			}
			kinds = append(kinds, fmt.Sprintf("%d %s", len(ids), perms))
		}
		sort.Strings(kinds)
		noun := "files"
		if len(docs) == 1 {
			noun = "file"
		}
		lines = append(lines, fmt.Sprintf("documents: %d %s shared with the app (paths not shown): %s", len(docs), noun, strings.Join(kinds, "; ")))
	}
	return strings.Join(lines, "\n")
}

// hidesData reports whether permission data is to be left out: byte
// strings (b'...', or arrays of bytes printed as 0x..) and anything long.
// Short values such as a single byte "0x00" or "yes" stay.
func hidesData(data string) bool {
	return len(data) > 40 || strings.Contains(data, "b'") || strings.Contains(data, `b"`) ||
		strings.Contains(data, "[0x") || strings.Contains(data, "@ay") || strings.Contains(data, "/")
}
