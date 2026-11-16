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

func TestEnv(t *testing.T) {
	// RPAK_JOBS with the number of downloads rpak runs for it.
	for v, want := range map[string]string{
		"": "RPAK_JOBS=(unset)  (4 downloads at a time)", "1": "RPAK_JOBS=1  (1 downloads at a time)",
		"16": "RPAK_JOBS=16  (16 downloads at a time)", "0": "RPAK_JOBS=0  (4 downloads at a time)",
		"17": "RPAK_JOBS=17  (4 downloads at a time)", "four": "RPAK_JOBS=four  (4 downloads at a time)",
	} {
		d := &doctor{cfg: Config{LookupEnv: func(k string) (string, bool) {
			if k == "RPAK_JOBS" && v != "" {
				return v, true
			}
			return "", false
		}}}
		found := false
		for _, it := range d.envSection().Items {
			found = found || it.Text == want
		}
		if !found {
			t.Errorf("RPAK_JOBS=%q: no %q in %+v", v, want, d.envSection().Items)
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
	has := func(items []Item, prefix string) bool {
		for _, it := range items {
			if strings.HasPrefix(it.Text, prefix) {
				return true
			}
		}
		return false
	}
	// The driver's version and the extensions, whatever they are.
	items := run("580.95.05", "org.freedesktop.Platform.GL.nvidia-575-64-05/x86_64/1.4", "org.freedesktop.Platform.GL32.nvidia-580-95-05/x86_64/1.4")
	for _, want := range []string{
		"NVIDIA driver 580.95.05",
		"GL extension (nvidia): org.freedesktop.Platform.GL.nvidia-575-64-05/x86_64/1.4  User installation",
		"GL extension (nvidia): org.freedesktop.Platform.GL32.nvidia-580-95-05/x86_64/1.4  User installation",
	} {
		if !has(items, want) {
			t.Errorf("no %q in %+v", want, items)
		}
	}
	if has(run("", "org.freedesktop.Platform.GL.default/x86_64/25.08"), "NVIDIA") {
		t.Error("NVIDIA line without the driver")
	}
}

func TestUserns(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "proc/sys/user"), 0o755)
	os.MkdirAll(filepath.Join(root, "proc/sys/kernel"), 0o755)
	os.WriteFile(filepath.Join(root, "proc/sys/user/max_user_namespaces"), []byte("63000\n"), 0o644)
	os.WriteFile(filepath.Join(root, "proc/sys/kernel/unprivileged_userns_clone"), []byte("0\n"), 0o644)
	d := &doctor{cfg: Config{Root: root, LookupEnv: func(string) (string, bool) { return "", false }}}
	var got []string
	for _, it := range d.systemSection(d.systemInfo(), nil).Items {
		if strings.Contains(it.Text, "userns") || strings.Contains(it.Text, "namespaces") {
			got = append(got, it.Text)
		}
	}
	// As they are, the missing one left out.
	if want := []string{"user.max_user_namespaces=63000", "kernel.unprivileged_userns_clone=0"}; !reflect.DeepEqual(got, want) {
		t.Errorf("userns: %q", got)
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
	check := func(path string, size int64) (ExtraDataCheck, string) {
		c := ExtraDataCheck{Source: metadata.Source{Name: "app.deb", Size: size, URI: srv.URL + path}}
		c.URL = CheckURL(context.Background(), srv.Client(), c.Source.URI, "test", 5*time.Second)
		return c, c.Text()
	}
	for _, c := range []struct {
		path   string
		size   int64
		text   string
		ranged bool
	}{
		// Both sizes, also where they are the same.
		{"/app.deb", 1000, "HTTP 200, 1.0 kB (pinned 1.0 kB)", false},
		{"/moved/app.deb", 1000, "HTTP 200, 1.0 kB (pinned 1.0 kB)", false},
		{"/missing.deb", 1000, "HTTP 404 (pinned 1.0 kB)", true},
		{"/new.deb", 1000, "HTTP 200, 1.2 kB (pinned 1.0 kB)", false},
		// HEAD refused: a one-byte GET, whose Content-Range has the size.
		{"/nohead/app.deb", 1000, "HTTP 206, 1.0 kB (pinned 1.0 kB)", true},
		// Different sizes that look the same rounded: in bytes.
		{"/nohead/app.deb", 1001, "HTTP 206, 1000 bytes (pinned 1001 bytes)", true},
	} {
		got, text := check(c.path, c.size)
		if text != c.text || got.URL.Ranged != c.ranged {
			t.Errorf("%s: %q ranged %v, want %q %v", c.path, text, got.URL.Ranged, c.text, c.ranged)
		}
	}
	unsized := ExtraDataCheck{Source: metadata.Source{Size: 1000}, URL: URLCheck{Status: 200, Length: -1}}
	if got := unsized.Text(); got != "HTTP 200, size not given (pinned 1.0 kB)" {
		t.Errorf("no size: %q", got)
	}

	// No answer at all: a connection error, named as such.
	srv.Close()
	if _, text := check("/app.deb", 1000); !strings.HasPrefix(text, "connection refused after ") || !strings.HasSuffix(text, "s (pinned 1.0 kB)") {
		t.Errorf("closed server: %q", text)
	}
}

func TestCheckURLTimeout(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	defer srv.Close()
	defer close(block)
	c := CheckURL(context.Background(), srv.Client(), srv.URL, "test", 100*time.Millisecond)
	if c.Outcome() != "timeout" || !strings.HasPrefix(c.Timed(), "timeout after 0.1") {
		t.Errorf("Outcome = %q, Timed = %q (%v)", c.Outcome(), c.Timed(), c.Err)
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
	// lines are the GPU, hybrid and NVIDIA lines, as printed.
	lines := func(items []Item) []string {
		var out []string
		for _, it := range items {
			if strings.HasPrefix(it.Text, "GPU") || strings.HasPrefix(it.Text, "hybrid") || strings.HasPrefix(it.Text, "NVIDIA") {
				out = append(out, "  "+it.Text)
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
			"  NVIDIA driver 580.95.05",
		}},
		{"NVIDIA without its version", "", []card{intel, {"card1", "10de", "2860", "nvidia", false}}, []string{
			"  GPU card0: Intel 8086:a7a0, driver i915 (boot)",
			"  GPU card1: NVIDIA 10de:2860, driver nvidia",
			"  hybrid graphics: Intel + NVIDIA",
			"  NVIDIA driver version: /proc/driver/nvidia/version does not exist",
		}},
		{"NVIDIA with nouveau", "", []card{{"card0", "10de", "1c82", "nouveau", true}}, []string{
			"  GPU card0: NVIDIA 10de:1c82, driver nouveau (boot)",
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
		// The driver's version without the cards (a container).
		{"no cards, NVIDIA driver", "580.95.05", nil, []string{"  GPU: none found in /sys/class/drm", "  NVIDIA driver 580.95.05"}},
	} {
		items := run(c.nvidia, c.cards...)
		if got := lines(items); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n%s\nwant:\n%s", c.name, strings.Join(got, "\n"), strings.Join(c.want, "\n"))
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
	runtimes := []listEntry{
		{"org.freedesktop.Platform/x86_64/24.08", "system,runtime" + eol},
		{"org.freedesktop.Platform.GL.default/x86_64/24.08", "system,runtime" + eol},
		{"org.freedesktop.Platform.VAAPI.Intel/x86_64/24.08", "system,runtime" + eol},
		{"org.kde.Platform/x86_64/5.15-24.08", "system,runtime,eol=We strongly recommend moving to the latest stable version"},
		{"org.gnome.Platform/x86_64/49", "system,runtime"},
	}
	// run reports on a system installation with the runtimes and apps, and
	// on an app org.example.App on runtime, if given.
	run := func(runtimes, apps []listEntry, runtime string, info map[string]string) Report {
		in := &instInfo{exists: true, runtimes: refList{refs: runtimes}, apps: refList{refs: apps}}
		in.inst.ID = "default"
		d := &doctor{cfg: Config{LookupEnv: func(string) (string, bool) { return "", false }}}
		sections := []Section{d.installationsSection([]*instInfo{in})}
		if runtime != "" {
			info["Ref"], info["Runtime"] = "app/org.example.App/x86_64/stable", runtime
			a := &appInfo{id: "org.example.App", checked: true, insts: []*appInst{{in: in, info: info}}}
			sections = append(sections, d.appSection(a, []*instInfo{in}, psInfo{}))
		}
		return Report{Sections: sections}
	}
	eolItems := func(r Report) []string {
		var out []string
		for _, s := range r.Sections {
			for _, it := range s.Items {
				if strings.Contains(it.Text, "end-of-life") {
					out = append(out, it.Text)
				}
			}
		}
		return out
	}

	// One line for the installation, all of them, runtimes first; the
	// app's runtime with its reason in the app's section.
	apps := []listEntry{{"org.example.Old/x86_64/stable", "system,current,eol=gone"}, {"org.example.App/x86_64/stable", "system,current"}}
	got := eolItems(run(runtimes, apps, "org.freedesktop.Platform/x86_64/24.08", map[string]string{}))
	want := []string{
		"end-of-life: org.freedesktop.Platform//24.08, org.freedesktop.Platform.GL.default//24.08, org.freedesktop.Platform.VAAPI.Intel//24.08, org.kde.Platform//5.15-24.08, org.example.Old//stable",
		"runtime: org.freedesktop.Platform/x86_64/24.08  installed in System installation (default)  end-of-life: org.freedesktop.Platform 24.08 is no longer receiving fixes",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("runtime end-of-life:\n%s", strings.Join(got, "\n"))
	}
	// The app itself, and its rename.
	got = eolItems(run(runtimes[4:], nil, "org.gnome.Platform/x86_64/49", map[string]string{"End-of-life": "use the new one", "End-of-life-rebase": "org.example.New"}))
	if want := []string{"ref: app/org.example.App/x86_64/stable  end-of-life: use the new one; end-of-life-rebase: org.example.New"}; !reflect.DeepEqual(got, want) {
		t.Errorf("app end-of-life:\n%s", strings.Join(got, "\n"))
	}
	// Nothing end-of-life: no line.
	if got := eolItems(run(runtimes[4:], nil, "", nil)); got != nil {
		t.Errorf("none: %q", got)
	}

	// More than listed: the first ones and a count; the same id//branch of
	// two arches once.
	var many []listEntry
	for i := range 11 {
		many = append(many, listEntry{fmt.Sprintf("org.example.R%d/x86_64/1", i), "runtime,eol=old"})
	}
	many = append(many, listEntry{"org.example.R0/i386/1", "runtime,eol=old"})
	if got := eolItems(run(many, nil, "", nil)); !reflect.DeepEqual(got, []string{"end-of-life: org.example.R0//1, org.example.R1//1, org.example.R2//1, org.example.R3//1, org.example.R4//1, org.example.R5//1, org.example.R6//1, org.example.R7//1, and 3 more"}) {
		t.Errorf("many:\n%s", strings.Join(got, "\n"))
	}
}
