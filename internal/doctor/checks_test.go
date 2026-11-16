package doctor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/flatpark/rpak/internal/metadata"
)

func TestSameExecutable(t *testing.T) {
	dir := t.TempDir()
	rpak := filepath.Join(dir, "rpak")
	other := filepath.Join(dir, "flatpak-real")
	os.WriteFile(rpak, []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(other, []byte("#!/bin/sh\n"), 0o755)
	link := filepath.Join(dir, "flatpak")
	if err := os.Symlink("rpak", link); err != nil {
		t.Fatal(err)
	}
	hard := filepath.Join(dir, "flatpak-hard")
	if err := os.Link(rpak, hard); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		flatpak string
		want    bool
	}{
		{rpak, true},
		{link, true}, // a symlink named flatpak in PATH
		{hard, true}, // a hard link: same inode
		{other, false},
		{filepath.Join(dir, "missing"), false},
		{"", false},
	} {
		if got := SameExecutable(c.flatpak, rpak); got != c.want {
			t.Errorf("SameExecutable(%s) = %v, want %v", c.flatpak, got, c.want)
		}
	}
	// Through a symlink to rpak, too.
	if !SameExecutable(rpak, link) {
		t.Error("SameExecutable(rpak, link to rpak) = false")
	}
}

func TestJobsAndDryRun(t *testing.T) {
	for _, c := range []struct {
		v  string
		n  int
		ok bool
	}{
		{"", 4, true}, {"1", 1, true}, {"16", 16, true}, {"8", 8, true},
		{"0", 4, false}, {"17", 4, false}, {"-2", 4, false}, {"four", 4, false}, {" 4", 4, false},
	} {
		if n, ok := JobsCheck(c.v); n != c.n || ok != c.ok {
			t.Errorf("JobsCheck(%q) = %d %v, want %d %v", c.v, n, ok, c.n, c.ok)
		}
	}
	for v, want := range map[string]bool{"": false, "0": false, "1": true, "yes": true} {
		if DryRun(v) != want {
			t.Errorf("DryRun(%q) = %v", v, !want)
		}
	}
}

func TestMissingDataDirs(t *testing.T) {
	user, system := "/home/a/.local/share/flatpak/exports/share", "/var/lib/flatpak/exports/share"
	for _, c := range []struct {
		value  string
		set    bool
		wanted []string
		want   []string
	}{
		{user + ":" + system + ":/usr/local/share:/usr/share", true, []string{user, system}, nil},
		// Trailing slashes and order do not matter.
		{"/usr/share/:" + system + "/:" + user + "/", true, []string{user, system}, nil},
		{"/usr/local/share:/usr/share", true, []string{user, system}, []string{user, system}},
		{system + ":/usr/share", true, []string{user}, []string{user}},
		// Unset or empty: /usr/local/share:/usr/share.
		{"", false, []string{system}, []string{system}},
		{"", true, []string{system}, []string{system}},
		// Nothing installed anywhere: nothing wanted.
		{"/usr/share", true, nil, nil},
	} {
		if got := MissingDataDirs(c.value, c.set, c.wanted); !reflect.DeepEqual(got, c.want) {
			t.Errorf("MissingDataDirs(%q, %v, %q) = %q, want %q", c.value, c.set, c.wanted, got, c.want)
		}
	}
}

func TestNvidia(t *testing.T) {
	for text, want := range map[string]string{
		"NVRM version: NVIDIA UNIX x86_64 Kernel Module  580.95.05  Tue Sep 23 09:46:56 UTC 2025\nGCC version:  gcc 14\n": "580.95.05",
		"NVRM version: NVIDIA UNIX Open Kernel Module for x86_64  570.144  Release Build  (dvs-builder@U16)\n":            "570.144",
		"NVRM version: NVIDIA UNIX Kernel Module for aarch64  535.247.01  Release Build\n":                                "535.247.01",
		"garbage\n": "",
	} {
		if got := NvidiaVersion(text); got != want {
			t.Errorf("NvidiaVersion(%q) = %q, want %q", text, got, want)
		}
	}
	if got := NvidiaExtension("580.95.05"); got != "org.freedesktop.Platform.GL.nvidia-580-95-05" {
		t.Errorf("NvidiaExtension = %q", got)
	}
	for id, want := range map[string]string{
		"org.freedesktop.Platform.GL.default":            "default",
		"org.freedesktop.Platform.GL32.default":          "default",
		"org.freedesktop.Platform.GL.nvidia-580-95-05":   "nvidia",
		"org.freedesktop.Platform.GL32.nvidia-580-95-05": "nvidia",
		"org.freedesktop.Platform.GL.mesa-git":           "other",
		"org.freedesktop.Platform":                       "",
		"org.freedesktop.Platform.VAAPI.Intel":           "",
	} {
		if got := GLKind(id); got != want {
			t.Errorf("GLKind(%s) = %q, want %q", id, got, want)
		}
	}

	// The whole check: the extension may be in any installation.
	run := func(driver string, runtimes ...string) []Item {
		root := t.TempDir()
		if driver != "" {
			os.MkdirAll(filepath.Join(root, "proc/driver/nvidia"), 0o755)
			os.WriteFile(filepath.Join(root, "proc/driver/nvidia/version"), []byte("NVRM version: NVIDIA UNIX x86_64 Kernel Module  "+driver+"  Release Build\n"), 0o644)
		}
		d := &doctor{cfg: Config{Root: root, LookupEnv: func(string) (string, bool) { return "", false }}}
		user := &instInfo{}
		user.inst.ID, user.inst.User = "user", true
		for _, r := range runtimes {
			user.runtimes.refs = append(user.runtimes.refs, listEntry{ref: r})
		}
		return d.systemSection(d.systemInfo(), []*instInfo{user, {}}).Items
	}
	level := func(items []Item, prefix string) (Level, bool) {
		for _, it := range items {
			if strings.HasPrefix(it.Text, prefix) {
				return it.Level, true
			}
		}
		return 0, false
	}
	items := run("580.95.05", "org.freedesktop.Platform.GL.default/x86_64/25.08", "org.freedesktop.Platform.GL.nvidia-580-95-05/x86_64/1.4")
	if l, ok := level(items, "NVIDIA driver 580.95.05"); !ok || l != OK {
		t.Errorf("matching extension: %v %v", l, ok)
	}
	if _, ok := level(items, "GL extension (nvidia): org.freedesktop.Platform.GL.nvidia-580-95-05/x86_64/1.4"); !ok {
		t.Errorf("nvidia extension not listed: %+v", items)
	}
	items = run("580.95.05", "org.freedesktop.Platform.GL.nvidia-575-64-05/x86_64/1.4", "org.freedesktop.Platform.GL32.nvidia-580-95-05/x86_64/1.4")
	if l, ok := level(items, "NVIDIA driver 580.95.05"); !ok || l != Fail {
		t.Errorf("other version, or only the 32-bit one: %v %v", l, ok)
	}
	if _, ok := level(run("", "org.freedesktop.Platform.GL.default/x86_64/25.08"), "NVIDIA"); ok {
		t.Error("NVIDIA line without the driver")
	}
}

func TestUserns(t *testing.T) {
	for _, c := range []struct {
		name, value string
		bad         bool
	}{
		{"user.max_user_namespaces", "0", true},
		{"user.max_user_namespaces", "15000", false},
		{"kernel.unprivileged_userns_clone", "0", true},
		{"kernel.unprivileged_userns_clone", "1", false},
		{"kernel.apparmor_restrict_unprivileged_userns", "1", false},
	} {
		if p, _ := UsernsProblem(c.name, c.value); (p != "") != c.bad {
			t.Errorf("UsernsProblem(%s, %s) = %q", c.name, c.value, p)
		}
	}

	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "proc/sys/user"), 0o755)
	os.MkdirAll(filepath.Join(root, "proc/sys/kernel"), 0o755)
	os.WriteFile(filepath.Join(root, "proc/sys/user/max_user_namespaces"), []byte("63000\n"), 0o644)
	os.WriteFile(filepath.Join(root, "proc/sys/kernel/unprivileged_userns_clone"), []byte("0\n"), 0o644)
	d := &doctor{cfg: Config{Root: root, LookupEnv: func(string) (string, bool) { return "", false }}}
	items := d.systemSection(d.systemInfo(), nil).Items
	var fails, infos []string
	for _, it := range items {
		switch {
		case it.Level == Fail:
			fails = append(fails, it.Text)
		case strings.Contains(it.Text, "user_namespaces"):
			infos = append(infos, it.Text)
		}
	}
	if len(fails) != 1 || !strings.Contains(fails[0], "unprivileged_userns_clone=0") {
		t.Errorf("fails: %q", fails)
	}
	if len(infos) != 1 || infos[0] != "user.max_user_namespaces=63000" {
		t.Errorf("infos: %q", infos)
	}
}

func TestPortalProcesses(t *testing.T) {
	proc := t.TempDir()
	uid := os.Getuid()
	add := func(pid, comm, argv0 string) {
		dir := filepath.Join(proc, pid)
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "comm"), []byte(comm+"\n"), 0o644)
		os.WriteFile(filepath.Join(dir, "cmdline"), []byte(argv0+"\x00--replace\x00"), 0o644)
	}
	add("10", "xdg-desktop-por", "/usr/libexec/xdg-desktop-portal")
	add("11", "xdg-desktop-por", "/usr/libexec/xdg-desktop-portal-gtk")
	add("12", "xdg-permission-", "/usr/libexec/xdg-permission-store")
	add("13", "bash", "bash")
	os.MkdirAll(filepath.Join(proc, "self"), 0o755)
	if got := PortalProcesses(proc, uid); !reflect.DeepEqual(got, []string{"xdg-desktop-portal", "xdg-desktop-portal-gtk"}) {
		t.Errorf("PortalProcesses = %q", got)
	}
	// Other users' processes do not count.
	if got := PortalProcesses(proc, uid+1); got != nil {
		t.Errorf("PortalProcesses(other uid) = %q", got)
	}
}

func TestRemotes(t *testing.T) {
	out := "flathub\thttps://dl.flathub.org/repo/\t\t1\n" +
		"flatpark\thttps://dl.flatpark.org/\tdisabled\t1\n" +
		"test\thttps://example.org/repo/\tno-enumerate,no-gpg-verify\t0\n" +
		"bundle-origin\tfile:///home/a/repo\tno-enumerate,no-gpg-verify\t0\n"
	remotes := ParseRemotes(out)
	want := []Remote{
		{Name: "flathub", URL: "https://dl.flathub.org/repo/", Priority: "1"},
		{Name: "flatpark", URL: "https://dl.flatpark.org/", Options: []string{"disabled"}, Priority: "1"},
		{Name: "test", URL: "https://example.org/repo/", Options: []string{"no-enumerate", "no-gpg-verify"}, Priority: "0"},
		{Name: "bundle-origin", URL: "file:///home/a/repo", Options: []string{"no-enumerate", "no-gpg-verify"}, Priority: "0"},
	}
	if !reflect.DeepEqual(remotes, want) {
		t.Fatalf("ParseRemotes = %+v", remotes)
	}
	if len(ParseRemotes("")) != 0 {
		t.Error("remotes from no output")
	}

	texts := func(ps []RemoteProblem) []string {
		var out []string
		for _, p := range ps {
			out = append(out, p.Text)
		}
		return out
	}
	// A local repository is normally unsigned; an http one should not be.
	got := texts(CheckRemotes(remotes, "--user"))
	if !reflect.DeepEqual(got, []string{
		"remote flatpark is disabled",
		"remote test is not GPG-verified (no-gpg-verify)",
	}) {
		t.Errorf("CheckRemotes = %q", got)
	}
	if ps := CheckRemotes(remotes, "--system"); !strings.Contains(ps[0].Hint, "remote-modify --system --enable flatpark") {
		t.Errorf("hint %q", ps[0].Hint)
	}

	// flathub and flatpark may be in any installation.
	user := []Remote{{Name: "flatpark", URL: "https://dl.flatpark.org/"}}
	system := []Remote{{Name: "flathub", URL: "https://dl.flathub.org/repo/"}}
	if got := MissingRemotes([][]Remote{user, system}); got != nil {
		t.Errorf("split between installations: %+v", got)
	}
	// A disabled one is still configured (and reported as disabled).
	if got := MissingRemotes([][]Remote{remotes}); got != nil {
		t.Errorf("all in one: %+v", got)
	}
	ps := MissingRemotes([][]Remote{user})
	if got := texts(ps); len(got) != 1 || !strings.HasPrefix(got[0], "no remote named flathub in any installation") {
		t.Errorf("without flathub: %q", got)
	}
	if ps[0].Hint != "flatpak remote-add --user --if-not-exists flathub https://dl.flathub.org/repo/flathub.flatpakrepo" {
		t.Errorf("hint %q", ps[0].Hint)
	}
	// Once, whatever the number of installations.
	if got := texts(MissingRemotes([][]Remote{nil, remotes[2:3], nil})); len(got) != 2 || got[1] != "no remote named flatpark in any installation" {
		t.Errorf("neither: %q", got)
	}
	// Only names count: flathub's URL under another name is no flathub.
	if got := texts(MissingRemotes([][]Remote{{{Name: "fh", URL: "https://dl.flathub.org/repo/"}, {Name: "flatpark"}}})); len(got) != 1 || !strings.HasPrefix(got[0], "no remote named flathub") {
		t.Errorf("by URL: %q", got)
	}
}

func TestList(t *testing.T) {
	list := parseList("org.freedesktop.Platform/x86_64/24.08\tuser,runtime,eol=org.freedesktop.Platform 24.08 is no longer supported, use 25.08\n" +
		"org.gnome.Platform/x86_64/47\tuser,eol=gone,eol-rebase=org.gnome.Platform/x86_64/48\n" +
		"org.freedesktop.Platform/x86_64/25.08\tuser\n\n")
	if len(list) != 3 {
		t.Fatalf("parseList: %+v", list)
	}
	for i, want := range []string{"org.freedesktop.Platform 24.08 is no longer supported, use 25.08", "gone", ""} {
		got, ok := list[i].eol()
		if got != want || ok != (want != "") {
			t.Errorf("%s: eol %q %v", list[i].ref, got, ok)
		}
	}
}

func TestOverrides(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "overrides")
	if global, has, ids := ReadOverrides(dir); has || global != "" || ids != nil {
		t.Error("overrides from a missing directory")
	}
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "global"), []byte("[Context]\nfilesystems=home;\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "org.b.App"), []byte("[Context]\nshared=network;\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "org.a.App"), []byte("[Environment]\nLANG=C\n"), 0o644)
	os.Mkdir(filepath.Join(dir, "a-dir"), 0o755)
	global, has, ids := ReadOverrides(dir)
	if !has || global != "[Context]\nfilesystems=home;\n" || !reflect.DeepEqual(ids, []string{"org.a.App", "org.b.App"}) {
		t.Errorf("ReadOverrides = %q %v %q", global, has, ids)
	}
	if text, ok := ReadOverride(dir, "org.b.App"); !ok || text != "[Context]\nshared=network;\n" {
		t.Errorf("ReadOverride = %q %v", text, ok)
	}
	for _, id := range []string{"org.c.App", "../overrides/org.a.App", "", ".."} {
		if _, ok := ReadOverride(dir, id); ok {
			t.Errorf("ReadOverride(%q) found something", id)
		}
	}
}

func TestLockHolders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locks")
	os.WriteFile(path, []byte(
		"1: POSIX  ADVISORY  WRITE 66194 00:24:48851 0 EOF\n"+
			"2: OFDLCK ADVISORY  READ  -1 00:24:777 0 EOF\n"+
			"3: OFDLCK ADVISORY  READ  4242 00:2a:888 0 EOF\n"+
			"3: -> OFDLCK ADVISORY  WRITE 4343 00:2a:888 0 EOF\n"+
			"4: FLOCK  ADVISORY  WRITE 99 08:02:888 0 EOF\n"), 0o644)
	for ino, want := range map[uint64][]int{48851: {66194}, 777: {-1}, 888: {4242, 99}, 5: nil} {
		if got, err := lockHolders(path, ino); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("lockHolders(%d) = %v %v, want %v", ino, got, err, want)
		}
	}
}

func TestVersions(t *testing.T) {
	for _, c := range []struct {
		version, latest string
		behind, ok      bool
	}{
		{"v0.1.0", "v0.1.1", true, true},
		{"v0.1.1", "v0.1.1", false, true},
		{"v0.1.1-dirty", "v0.1.1", false, true},
		{"v0.1.1-3-gabcdef0", "v0.1.1", false, true},
		{"v0.1.0-3-gabcdef0-dirty", "v0.1.1", true, true},
		{"v0.2.0", "v0.10.0", true, true},
		{"v1.0", "v0.9.9", false, true},
		{"dev", "v0.1.1", false, false},
		{"34a4f7d", "v0.1.1", false, false},
		{"v0.1.1", "", false, false},
	} {
		if behind, ok := Behind(c.version, c.latest); behind != c.behind || ok != c.ok {
			t.Errorf("Behind(%s, %s) = %v %v", c.version, c.latest, behind, ok)
		}
	}
}

// extraDataServer serves files by path: HEAD and ranges like a normal
// server, or, for paths under /nohead/, 405 for HEAD.
func extraDataServer(t *testing.T, files map[string]int) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.UserAgent() != "rpak/test" {
			http.Error(w, "bad user agent "+r.UserAgent(), http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/moved/") {
			http.Redirect(w, r, strings.TrimPrefix(r.URL.Path, "/moved"), http.StatusFound)
			return
		}
		size, ok := files[r.URL.Path]
		if !ok && strings.HasPrefix(r.URL.Path, "/repo/") {
			// A real remote, see internal/summary/testdata.
			http.ServeFile(w, r, filepath.Join("../summary/testdata", r.URL.Path))
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/nohead/") && r.Method == http.MethodHead {
			http.Error(w, "no HEAD", http.StatusMethodNotAllowed)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/nohead/") {
			if r.Header.Get("Range") != "bytes=0-0" {
				http.Error(w, "want a range", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", size))
			w.WriteHeader(http.StatusPartialContent)
			w.Write([]byte{'x'})
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(size))
		if r.Method != http.MethodHead {
			w.Write(make([]byte, size))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestExtraDataCheck(t *testing.T) {
	srv := extraDataServer(t, map[string]int{"/app.deb": 1000, "/nohead/app.deb": 1000, "/new.deb": 1200})
	check := func(path string, size int64) (ExtraDataCheck, Level, string) {
		c := ExtraDataCheck{Source: metadata.Source{Name: "app.deb", Size: size, URI: srv.URL + path}}
		c.URL = CheckURL(context.Background(), srv.Client(), c.Source.URI, "test", 5*time.Second)
		level, what := c.Judge()
		return c, level, what
	}
	for _, c := range []struct {
		path   string
		level  Level
		what   string
		ranged bool
	}{
		{"/app.deb", OK, "HTTP 200, 1.0 kB as pinned", false},
		{"/moved/app.deb", OK, "HTTP 200, 1.0 kB as pinned", false},
		{"/missing.deb", Fail, "HTTP 404", true},
		{"/new.deb", Fail, "the server has 1.2 kB, pinned 1.0 kB: the developer replaced the file and the pin is not updated yet", false},
		// HEAD refused: a one-byte GET, whose Content-Range has the size.
		{"/nohead/app.deb", OK, "HTTP 206, 1.0 kB as pinned", true},
	} {
		got, level, what := check(c.path, 1000)
		if level != c.level || what != c.what || got.URL.Ranged != c.ranged {
			t.Errorf("%s: %v %q ranged %v, want %v %q %v", c.path, level, what, got.URL.Ranged, c.level, c.what, c.ranged)
		}
	}
	if _, level, what := check("/nohead/app.deb", 999); level != Fail || !strings.Contains(what, "replaced") {
		t.Errorf("ranged size mismatch: %v %q", level, what)
	}

	// No answer at all: a connection error, named as such.
	srv.Close()
	if _, level, what := check("/app.deb", 1000); level != Fail || what != "connection refused" {
		t.Errorf("closed server: %v %q", level, what)
	}
}

func TestCheckURLTimeout(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	defer srv.Close()
	defer close(block)
	c := CheckURL(context.Background(), srv.Client(), srv.URL, "test", 100*time.Millisecond)
	if c.Problem() != "timeout" {
		t.Errorf("Problem = %q (%v)", c.Problem(), c.Err)
	}
}

func TestPermissionStore(t *testing.T) {
	// As flatpak prints them; the documents rows name files the user
	// opened, in GVariant's escapes for non-ASCII bytes.
	out := "documents\to5t_mFGpGh_5vFFm517hCg\tcom.example.App\tread,write,grant-permissions\t(b'/home/a/Documents/GenOffice/\\xe5\\xb7\\xa5\\xe8\\xb5\\x84.xlsx', 54, 7655979, 0, [0x2b, 0xd2, 0x74, 0x00])\n" +
		"documents\tQx9dAAAAAAAAAAAAAAAAAg\tcom.example.App\tread\t(b'/home/a/books/Some Title.epub', 54, 1, 0, [0x00])\n" +
		"documents\tZz1bBBBBBBBBBBBBBBBBBg\tcom.example.App\tread,write,grant-permissions\t(b'/tmp/out.png', 54, 2, 0, [0x00])\n" +
		"screencast\t95c4ac0c-287e-4fc4-9995-1464a36278c3\tcom.example.App\tyes\t('wlroots', 1, <{'output_name': <'DP-1'>, 'path': <'/home/a/x'>}>)\n" +
		"devices\tcamera\tcom.example.App\tyes\t0x00\n" +
		"notifications\tnotification\tcom.example.App\tyes\n"
	got := PermissionStore(out)
	want := "screencast\t95c4ac0c-287e-4fc4-9995-1464a36278c3\tcom.example.App\tyes\t(data omitted)\n" +
		"devices\tcamera\tcom.example.App\tyes\t0x00\n" +
		"notifications\tnotification\tcom.example.App\tyes\n" +
		"documents: 3 files shared with the app (paths not shown): 1 read; 2 read,write,grant-permissions"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	for _, leak := range []string{"Documents", "GenOffice", "xlsx", "\\xe5", "epub", "Title", "/tmp", "out.png", "o5t_mFGpGh", "Qx9dAAAA", "DP-1", "/home"} {
		if strings.Contains(got, leak) {
			t.Errorf("%q in the output", leak)
		}
	}
	if got := PermissionStore("documents\tabc\tcom.example.App\tread\t(b'/x', 1)\n"); got != "documents: 1 file shared with the app (paths not shown): 1 read" {
		t.Errorf("one file: %q", got)
	}
	if got := PermissionStore(""); got != "" {
		t.Errorf("nothing: %q", got)
	}
}

func TestGPUs(t *testing.T) {
	// card adds /sys/class/drm/<card>; driver "" is no driver link.
	type card struct {
		name, vendor, device, driver string
		boot                         bool
	}
	run := func(nvidia string, cards ...card) []Item {
		root := t.TempDir()
		drm := filepath.Join(root, "sys/class/drm")
		os.MkdirAll(drm, 0o755)
		for _, c := range cards {
			dev := filepath.Join(drm, c.name, "device")
			os.MkdirAll(dev, 0o755)
			os.WriteFile(filepath.Join(dev, "vendor"), []byte("0x"+c.vendor+"\n"), 0o644)
			os.WriteFile(filepath.Join(dev, "device"), []byte("0x"+c.device+"\n"), 0o644)
			boot := "0\n"
			if c.boot {
				boot = "1\n"
			}
			os.WriteFile(filepath.Join(dev, "boot_vga"), []byte(boot), 0o644)
			if c.driver != "" {
				// As in sysfs: a link to the driver's directory, which
				// need not exist here.
				os.Symlink("../../../../bus/pci/drivers/"+c.driver, filepath.Join(dev, "driver"))
			}
		}
		if nvidia != "" {
			os.MkdirAll(filepath.Join(root, "proc/driver/nvidia"), 0o755)
			os.WriteFile(filepath.Join(root, "proc/driver/nvidia/version"), []byte("NVRM version: NVIDIA UNIX x86_64 Kernel Module  "+nvidia+"  Release Build\n"), 0o644)
		}
		d := &doctor{cfg: Config{Root: root, LookupEnv: func(string) (string, bool) { return "", false }}}
		return d.systemSection(d.systemInfo(), nil).Items
	}
	// lines are the GPU, hybrid and NVIDIA lines, marked as printed.
	lines := func(items []Item) []string {
		var out []string
		for _, it := range items {
			if strings.HasPrefix(it.Text, "GPU") || strings.HasPrefix(it.Text, "hybrid") || strings.HasPrefix(it.Text, "NVIDIA") {
				out = append(out, map[Level]string{Info: " ", OK: "✓", Warn: "!", Fail: "✗"}[it.Level]+" "+it.Text)
			}
		}
		return out
	}
	intel := card{"card0", "8086", "a7a0", "i915", true}
	for _, c := range []struct {
		name   string
		nvidia string
		cards  []card
		want   []string
	}{
		{"Intel", "", []card{intel}, []string{"  GPU card0: Intel 8086:a7a0, driver i915 (boot)"}},
		{"Intel and NVIDIA", "580.95.05", []card{{"card1", "10de", "2860", "nvidia", false}, intel}, []string{
			"  GPU card0: Intel 8086:a7a0, driver i915 (boot)",
			"  GPU card1: NVIDIA 10de:2860, driver nvidia",
			"  hybrid graphics: Intel + NVIDIA",
			// The extension check as before.
			"✗ NVIDIA driver 580.95.05, but org.freedesktop.Platform.GL.nvidia-580-95-05 is not installed: apps fall back to software rendering or do not start",
		}},
		{"NVIDIA without its version", "", []card{intel, {"card1", "10de", "2860", "nvidia", false}}, []string{
			"  GPU card0: Intel 8086:a7a0, driver i915 (boot)",
			"  GPU card1: NVIDIA 10de:2860, driver nvidia",
			"  hybrid graphics: Intel + NVIDIA",
			"! NVIDIA card1 uses the nvidia driver, but /proc/driver/nvidia/version does not exist: the GL.nvidia extension apps need is unknown",
		}},
		{"NVIDIA with nouveau", "", []card{{"card0", "10de", "1c82", "nouveau", true}}, []string{
			"  GPU card0: NVIDIA 10de:1c82, driver nouveau (boot)",
			"  NVIDIA card0 uses nouveau, which Mesa drives through GL.default: no GL.nvidia extension is needed",
		}},
		{"AMD", "", []card{{"card1", "1002", "73df", "amdgpu", true}}, []string{"  GPU card1: AMD 1002:73df, driver amdgpu (boot)"}},
		{"other vendor", "", []card{{"card0", "1af4", "1050", "virtio_gpu", false}}, []string{"  GPU card0: 1af4:1050, driver virtio_gpu"}},
		// Connectors are left out, and so is renderD128; card10 after card2.
		{"connectors", "", []card{{"card10", "1002", "73df", "amdgpu", false}, {"card10-DP-1", "", "", "", false}, {"card0-HDMI-A-1", "", "", "", false}, {"renderD128", "", "", "", false}, {"card2", "8086", "a7a0", "", false}}, []string{
			"  GPU card2: Intel 8086:a7a0, no driver",
			"  GPU card10: AMD 1002:73df, driver amdgpu",
		}},
		// One card with a driver is no hybrid.
		{"no driver", "", []card{intel, {"card1", "10de", "2860", "", false}}, []string{
			"  GPU card0: Intel 8086:a7a0, driver i915 (boot)",
			"  GPU card1: NVIDIA 10de:2860, no driver",
		}},
		{"no cards", "", nil, []string{"  GPU: none found in /sys/class/drm"}},
	} {
		items := run(c.nvidia, c.cards...)
		if got := lines(items); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n%s\nwant:\n%s", c.name, strings.Join(got, "\n"), strings.Join(c.want, "\n"))
		}
		for _, it := range items {
			if it.Level == Warn && it.Hint == "" {
				t.Errorf("%s: %q has no hint", c.name, it.Text)
			}
		}
	}
	// No /sys at all.
	d := &doctor{cfg: Config{Root: t.TempDir(), LookupEnv: func(string) (string, bool) { return "", false }}}
	if got := lines(d.systemSection(d.systemInfo(), nil).Items); !reflect.DeepEqual(got, []string{"  GPU: none found in /sys/class/drm"}) {
		t.Errorf("no /sys: %q", got)
	}
}

func TestEOL(t *testing.T) {
	if got := ShortRef("org.freedesktop.Platform/x86_64/24.08"); got != "org.freedesktop.Platform//24.08" {
		t.Errorf("ShortRef = %q", got)
	}
	if got := ShortRef("app/org.example.App/x86_64/stable"); got != "org.example.App//stable" {
		t.Errorf("ShortRef(app/...) = %q", got)
	}
	eol := ",eol=org.freedesktop.Platform 24.08 is no longer receiving fixes"
	unrelated := []listEntry{
		{"org.freedesktop.Platform/x86_64/24.08", "system,runtime" + eol},
		{"org.freedesktop.Platform.GL.default/x86_64/24.08", "system,runtime" + eol},
		{"org.freedesktop.Platform.GL.default/x86_64/24.08extra", "system,runtime" + eol},
		{"org.freedesktop.Platform.VAAPI.Intel/x86_64/24.08", "system,runtime" + eol},
		{"org.kde.Platform/x86_64/5.15-24.08", "system,runtime,eol=We strongly recommend moving to the latest stable version"},
		{"org.gnome.Platform/x86_64/49", "system,runtime"},
	}
	// run reports on a system installation with the runtimes and an app
	// org.example.App on runtime, if given; apps are the ids checked.
	run := func(runtimes []listEntry, runtime string, apps ...string) Report {
		in := &instInfo{exists: true, writable: true, free: lowSpace, runtimes: refList{refs: runtimes}, remotes: []Remote{{Name: "flathub"}, {Name: "flatpark"}}}
		in.inst.ID = "default"
		var checked []*appInfo
		for _, id := range apps {
			a := &appInfo{id: id, checked: true}
			if runtime != "" {
				a.insts = []*appInst{{in: in, info: map[string]string{"Ref": "app/" + id + "/x86_64/stable", "Runtime": runtime}}}
				in.apps.refs = append(in.apps.refs, listEntry{ref: id + "/x86_64/stable", options: "system,current"})
			}
			checked = append(checked, a)
		}
		d := &doctor{cfg: Config{Apps: apps, LookupEnv: func(string) (string, bool) { return "", false }}}
		sections := []Section{d.installationsSection([]*instInfo{in}, checked)}
		for _, a := range checked {
			sections = append(sections, d.appSection(a, []*instInfo{in}, psInfo{}))
		}
		return Report{Sections: sections}
	}
	eolItems := func(r Report) []string {
		var out []string
		for _, s := range r.Sections {
			for _, it := range s.Items {
				if strings.Contains(it.Text, "end-of-life") {
					out = append(out, map[Level]string{Info: " ", Warn: "!"}[it.Level]+" "+it.Text)
				}
			}
		}
		return out
	}
	const five = "org.freedesktop.Platform//24.08, org.freedesktop.Platform.GL.default//24.08, org.freedesktop.Platform.GL.default//24.08extra, org.freedesktop.Platform.VAAPI.Intel//24.08, org.kde.Platform//5.15-24.08"

	// The app's runtime is end-of-life: a problem in its section, with the
	// reason; the others one line, not a problem.
	r := run(unrelated, "org.freedesktop.Platform/x86_64/24.08", "org.example.App")
	want := []string{
		"  4 end-of-life runtimes not used by the apps checked: org.freedesktop.Platform.GL.default//24.08, org.freedesktop.Platform.GL.default//24.08extra, org.freedesktop.Platform.VAAPI.Intel//24.08, org.kde.Platform//5.15-24.08",
		"! runtime org.freedesktop.Platform/x86_64/24.08 (System installation (default)) is end-of-life: org.freedesktop.Platform 24.08 is no longer receiving fixes",
	}
	if got := eolItems(r); !reflect.DeepEqual(got, want) {
		t.Errorf("app's runtime end-of-life:\n%s", strings.Join(got, "\n"))
	}
	if ps := r.Problems(); len(ps) != 1 || !strings.HasPrefix(ps[0].Text, "runtime org.freedesktop.Platform/x86_64/24.08") {
		t.Errorf("problems: %+v", ps)
	}

	// Only end-of-life refs the app does not use: no problem.
	r = run(unrelated, "org.gnome.Platform/x86_64/49", "org.example.App")
	if got := eolItems(r); !reflect.DeepEqual(got, []string{"  5 end-of-life runtimes not used by the apps checked: " + five}) {
		t.Errorf("unrelated end-of-life:\n%s", strings.Join(got, "\n"))
	}
	if ps := r.Problems(); len(ps) != 0 {
		t.Errorf("problems: %+v", ps)
	}
	// An app that is not installed uses nothing.
	if got := eolItems(run(unrelated, "", "org.example.Gone")); !reflect.DeepEqual(got, []string{"  5 end-of-life runtimes not used by the apps checked: " + five}) {
		t.Errorf("app not installed:\n%s", strings.Join(got, "\n"))
	}

	// No app: one problem per installation, without the reasons.
	r = run(unrelated, "")
	if got := eolItems(r); !reflect.DeepEqual(got, []string{"! 5 end-of-life runtimes: " + five}) {
		t.Errorf("no app:\n%s", strings.Join(got, "\n"))
	}
	ps := r.Problems()
	if len(ps) != 1 || ps[0].Context != "System installation (default)" ||
		ps[0].Hint != `flatpak update --system; if the apps still need it, they have to move on first; "flatpak uninstall --system --unused" removes unused runtimes` {
		t.Errorf("problems: %+v", ps)
	}

	// More than eolListed: the first ones and a count; one is one.
	var many []listEntry
	for i := range 11 {
		many = append(many, listEntry{fmt.Sprintf("org.example.R%d/x86_64/1", i), "runtime,eol=old"})
	}
	if got := eolItems(run(many, "")); !reflect.DeepEqual(got, []string{"! 11 end-of-life runtimes: org.example.R0//1, org.example.R1//1, org.example.R2//1, org.example.R3//1, org.example.R4//1, org.example.R5//1, org.example.R6//1, org.example.R7//1, and 3 more"}) {
		t.Errorf("many:\n%s", strings.Join(got, "\n"))
	}
	if got := eolItems(run(many[:1], "")); !reflect.DeepEqual(got, []string{"! 1 end-of-life runtime: org.example.R0//1"}) {
		t.Errorf("one:\n%s", strings.Join(got, "\n"))
	}
	// Apps, besides runtimes; the same id//branch of two arches once.
	if got := EOLSummary([]string{"a//1"}, []string{"b//stable", "c//stable"}, true); got != "1 end-of-life runtime and 2 apps other than the apps checked and their runtimes: a//1, b//stable, c//stable" {
		t.Errorf("EOLSummary with apps: %q", got)
	}
	twoArches := []listEntry{{"org.example.R/x86_64/1", "runtime,eol=old"}, {"org.example.R/i386/1", "runtime,eol=old"}}
	if got := eolItems(run(twoArches, "")); !reflect.DeepEqual(got, []string{"! 1 end-of-life runtime: org.example.R//1"}) {
		t.Errorf("two arches:\n%s", strings.Join(got, "\n"))
	}
}
