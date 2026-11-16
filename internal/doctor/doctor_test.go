package doctor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRunner answers flatpak commands from a table, keyed by the
// space-joined arguments; anything else fails as flatpak does.
type fakeRunner struct {
	mu    sync.Mutex
	out   map[string]string
	calls []string
}

func (f *fakeRunner) Output(_ context.Context, argv ...string) ([]byte, error) {
	key := strings.Join(argv, " ")
	f.mu.Lock()
	f.calls = append(f.calls, key)
	f.mu.Unlock()
	if out, ok := f.out[key]; ok {
		if strings.HasPrefix(out, "ERROR ") {
			return nil, errors.New("flatpak " + argv[0] + ": " + strings.TrimPrefix(out, "ERROR "))
		}
		return []byte(out), nil
	}
	return nil, fmt.Errorf("flatpak %s: error: %s not installed", argv[0], argv[len(argv)-1])
}

func mkfile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func find(r Report, title, prefix string) (Item, bool) {
	for _, s := range r.Sections {
		if s.Title != title {
			continue
		}
		for _, it := range s.Items {
			if strings.HasPrefix(it.Text, prefix) {
				return it, true
			}
		}
	}
	return Item{}, false
}

func TestRun(t *testing.T) {
	home := filepath.Join(t.TempDir(), "alice")
	userDir := filepath.Join(home, ".local/share/flatpak")
	sysDir := t.TempDir()
	root := t.TempDir()
	bin := t.TempDir()
	flatpak, rpak := filepath.Join(bin, "flatpak"), filepath.Join(bin, "rpak")
	mkfile(t, flatpak, "#!/bin/sh\n")
	mkfile(t, rpak, "#!/bin/sh\n")

	// The remote is the repository of internal/summary/testdata, whose
	// org.example.Extra//stable has app-1.0.deb, of 1000 bytes.
	srv := extraDataServer(t, map[string]int{"/chrome.deb": 1000, "/other.deb": 500, "/flatpark.flatpakrepo": 10, "/app-1.0.deb": 1000})

	// The user installation: a repository, a global override, an app
	// override, a partial download.
	mkfile(t, filepath.Join(userDir, "repo/config"), "[core]\n")
	mkfile(t, filepath.Join(userDir, "overrides/global"), "[Context]\nfilesystems="+home+"/Documents;\n")
	mkfile(t, filepath.Join(userDir, "overrides/org.example.Extra"), "[Environment]\nHOME_COPY="+home+"\n")
	mkfile(t, filepath.Join(userDir, "extra-data", strings.Repeat("a", 64), "chrome.deb"), strings.Repeat("x", 1000))
	mkfile(t, filepath.Join(userDir, "extra-data", strings.Repeat("b", 64), "other.deb.part"), "xx")
	mkfile(t, filepath.Join(home, ".var/app/org.example.Extra/config"), "12345")
	mkfile(t, filepath.Join(root, "etc/os-release"), "NAME=Test\nPRETTY_NAME=\"Test OS 1\"\n")
	mkfile(t, filepath.Join(root, "proc/sys/user/max_user_namespaces"), "100\n")

	meta := func(name, sum string, size int) string {
		return fmt.Sprintf("[Application]\nname=org.example.Extra\nruntime=org.freedesktop.Platform/x86_64/25.08\n\n[Extra Data]\nname=%s\nchecksum=%s\nsize=%d\nuri=%s/%s\n", name, sum, size, srv.URL, name)
	}
	runner := &fakeRunner{out: map[string]string{
		"--version":      "Flatpak 1.16.1\n",
		"--default-arch": "x86_64\n",
		"remotes --user --columns=name,url,options,priority":   "flatpark\t" + srv.URL + "/repo/\t\t1\nlocal\tfile://" + home + "/repo\tno-gpg-verify\t0\n",
		"remotes --system --columns=name,url,options,priority": "",
		"list --user --app --columns=ref,options":              "org.example.Extra/x86_64/stable\tuser,current\ncom.example.Other/x86_64/stable\tuser,current\n",
		"list --user --runtime --columns=ref,options":          "org.freedesktop.Platform/x86_64/25.08\tuser,runtime,eol=too old, really\norg.freedesktop.Platform.GL.default/x86_64/25.08\tuser,runtime\n",
		"list --system --app --columns=ref,options":            "",
		"list --system --runtime --columns=ref,options":        "",
		"ps --columns=application,pid":                         "org.example.Extra\t4242\n",
		"info --user -- org.example.Extra":                     "\nChrome - browser\n\n          ID: org.example.Extra\n         Ref: app/org.example.Extra/x86_64/stable\n     Version: 1.0\n      Origin: flatpark\n     Runtime: org.freedesktop.Platform/x86_64/25.08\n      Commit: abc\n",
		"info --user --show-permissions -- org.example.Extra":  "[Context]\nfilesystems=" + home + "/Downloads;\n",
		"info --user --show-metadata -- org.example.Extra": meta("chrome.deb", strings.Repeat("a", 64), 1000) +
			fmt.Sprintf("name1=other.deb\nchecksum1=%s\nsize1=400\nuri1=%s/other.deb\n", strings.Repeat("b", 64), srv.URL),
		"permission-show -- org.example.Extra": "",
		"permission-show -- org.example.Gone":  "ERROR error: no session bus",
	}}
	env := map[string]string{
		"FLATPAK_SYSTEM_DIR": sysDir,
		"FLATPAK_CONFIG_DIR": t.TempDir(),
		"XDG_DATA_DIRS":      "/usr/share",
		"RPAK_JOBS":          "99",
		"https_proxy":        "http://alice:hunter2@proxy.example:3128",
	}
	cfg := Config{
		Version:    "test", // the User-Agent extraDataServer wants
		Executable: rpak,
		Flatpak:    flatpak,
		Runner:     runner,
		Client:     toServer(srv),
		Proxy: func(r *http.Request) (*url.URL, error) {
			if r.URL.Scheme == "https" {
				return url.Parse(env["https_proxy"])
			}
			return nil, nil
		},
		LookupEnv:    func(k string) (string, bool) { v, ok := env[k]; return v, ok },
		Home:         home,
		Users:        []string{"alice"},
		Root:         root,
		Uid:          os.Getuid(),
		Kernel:       func() string { return "6.1.0-test" },
		Probes:       []string{srv.URL + "/flatpark.flatpakrepo", srv.URL + "/repo/summary"},
		Apps:         []string{"org.example.Extra", "org.example.Plain", "org.example.Gone"},
		Arch:         "x86_64",
		NetTimeout:   5 * time.Second,
		DuTimeout:    5 * time.Second,
		CleanTimeout: 5 * time.Second,
	}
	before := snapshot(t, home, sysDir, root)
	r := Run(context.Background(), cfg)
	if after := snapshot(t, home, sysDir, root); after != before {
		t.Errorf("files changed:\n%s\n---\n%s", before, after)
	}

	var titles []string
	for _, s := range r.Sections {
		titles = append(titles, s.Title)
	}
	if got := strings.Join(titles, "|"); got != "rpak and flatpak|Environment|System|Installations|Extra-data directory|Network|App org.example.Extra|App org.example.Plain|App org.example.Gone" {
		t.Errorf("sections: %s", got)
	}

	for _, c := range []struct {
		title, prefix string
		level         Level
	}{
		{"rpak and flatpak", "Flatpak 1.16.1", OK},
		{"rpak and flatpak", "distribution: Test OS 1", Info},
		{"Environment", "RPAK_JOBS=99 is not a number", Warn},
		{"Environment", "XDG_DATA_DIRS lacks ~/.local/share/flatpak/exports/share", Warn},
		{"Environment", "https_proxy=http://***@proxy.example:3128", Info},
		{"System", "xdg-desktop-portal is not running", Warn},
		{"Installations", "remote local  file://~/repo  (priority 0, no-gpg-verify)", Info},
		{"Installations", "no remote named flathub in any installation", Warn},
		{"Installations", "org.freedesktop.Platform/x86_64/25.08 is end-of-life: too old, really", Warn},
		{"Installations", "global override (~/.local/share/flatpak/overrides/global)", Warn},
		{"Installations", "apps with an override: org.example.Extra", Info},
		{"Installations", "2 apps, 2 runtimes", Info},
		{"Extra-data directory", "1 partial downloads (.part), 2 bytes", Warn},
		{"Network", "proxy for https: http://***@proxy.example:3128, for http: none", Info},
		{"Network", srv.URL + "/repo/summary  HTTP 200", OK},
		{"Network", srv.URL + "/flatpark.flatpakrepo  HTTP 200", OK},
		{"App org.example.Extra", "runtime org.freedesktop.Platform/x86_64/25.08 (User installation) is end-of-life", Warn},
		// The remote has another commit than the installed one: both.
		{"App org.example.Extra", "extra-data app.deb  https://example.org/app-1.0.deb  HTTP 200, 1.0 kB as pinned (remote commit, pending update)", OK},
		{"App org.example.Extra", "extra-data chrome.deb  " + srv.URL + "/chrome.deb  HTTP 200, 1.0 kB as pinned (cached) (installed commit)", OK},
		{"App org.example.Extra", "extra-data other.deb  " + srv.URL + "/other.deb  the server has 500 bytes, pinned 400 bytes", Fail},
		// Not installed, found in the remote.
		{"App org.example.Plain", "not installed in any installation", Info},
		{"App org.example.Plain", "no extra-data (remote flatpark (User installation), branch stable)", Info},
		{"App org.example.Extra", "override " + "~/.local/share/flatpak/overrides/org.example.Extra", Info},
		{"App org.example.Extra", "also subject to the global override of the user installation", Info},
		{"App org.example.Extra", "permission store (flatpak permission-show): none", Info},
		{"App org.example.Extra", "~/.var/app/org.example.Extra: 5 bytes", Info},
		{"App org.example.Extra", "running: pid 4242", Info},
		{"App org.example.Gone", "org.example.Gone is not found in any remote", Warn},
		{"App org.example.Gone", "cannot read the permission store: flatpak permission-show: error: no session bus", Warn},
	} {
		it, ok := find(r, c.title, c.prefix)
		if !ok {
			t.Errorf("%s: no item %q", c.title, c.prefix)
			continue
		}
		if it.Level != c.level {
			t.Errorf("%s: %q is %v, want %v", c.title, it.Text, it.Level, c.level)
		}
		if (it.Level == Warn || it.Level == Fail) && it.Hint == "" {
			t.Errorf("%s: %q has no hint", c.title, it.Text)
		}
	}
	// The remote's file:// URL is not checked, its summary is checked once.
	if it, ok := find(r, "Network", "file://~/repo  not checked"); !ok || it.Level != Info {
		t.Errorf("file remote: %+v %v", it, ok)
	}
	if !r.Failed() {
		t.Error("not Failed with a replaced extra-data file")
	}

	// Nothing of the home directory or the user name, and no password.
	for _, s := range r.Sections {
		for _, it := range s.Items {
			for _, text := range []string{it.Text, it.Hint, it.Block, it.Context} {
				if strings.Contains(text, home) || strings.Contains(text, "alice") || strings.Contains(text, "hunter2") {
					t.Errorf("%s: not redacted: %q", s.Title, text)
				}
			}
		}
	}
	if it, _ := find(r, "Installations", "global override"); it.Block != "[Context]\nfilesystems=~/Documents;" {
		t.Errorf("global override block %q", it.Block)
	}
	if it, _ := find(r, "App org.example.Extra", "permissions (flatpak info"); it.Block != "[Context]\nfilesystems=~/Downloads;" {
		t.Errorf("permissions block %q", it.Block)
	}

	// Nothing about the unused system installation, and a local remote
	// without GPG verification is not a problem.
	for _, p := range r.Problems() {
		if strings.HasPrefix(p.Context, "System installation") || strings.Contains(p.Text, "remote local") {
			t.Errorf("problem: %s: %s", p.Context, p.Text)
		}
	}
	// Read-only: besides the files (above), no flatpak command but
	// queries ran, and not remote-info, which caches the summary.
	for _, call := range runner.calls {
		verb := strings.Fields(call)[0]
		switch verb {
		case "--version", "--default-arch", "remotes", "list", "ps", "info", "permission-show":
		default:
			t.Errorf("ran flatpak %s", call)
		}
	}
}

// toServer is a client that sends every request to srv, whatever the
// host: remotes and extra-data have the URLs of real servers.
func toServer(srv *httptest.Server) *http.Client {
	base, _ := url.Parse(srv.URL)
	return &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.URL.Scheme, r.URL.Host = base.Scheme, base.Host
		return srv.Client().Transport.RoundTrip(r)
	})}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// snapshot lists every file below the directories with size, mode and
// modification time.
func snapshot(t *testing.T, dirs ...string) string {
	var b strings.Builder
	for _, dir := range dirs {
		filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&b, "%s %d %v %d\n", p, info.Size(), info.Mode(), info.ModTime().UnixNano())
			return nil
		})
	}
	return b.String()
}

func TestRunFlatpakIsRpak(t *testing.T) {
	bin := t.TempDir()
	rpak := filepath.Join(bin, "rpak")
	mkfile(t, rpak, "#!/bin/sh\n")
	link := filepath.Join(bin, "flatpak")
	os.Symlink(rpak, link)
	runner := &fakeRunner{}
	cfg := Config{
		Version: "dev", Executable: rpak, Flatpak: link, Runner: runner,
		LookupEnv: func(k string) (string, bool) {
			if k == "FLATPAK_USER_DIR" {
				return filepath.Join(bin, "user"), true
			}
			if k == "FLATPAK_SYSTEM_DIR" {
				return filepath.Join(bin, "system"), true
			}
			return "", false
		},
		Home: bin, Root: t.TempDir(), Apps: []string{"org.example.App"},
	}
	r := Run(context.Background(), cfg)
	if it, ok := find(r, "rpak and flatpak", "flatpak in PATH is rpak itself"); !ok || it.Level != Fail {
		t.Errorf("self: %+v %v", it, ok)
	}
	if len(runner.calls) != 0 {
		t.Errorf("flatpak ran: %q", runner.calls)
	}
	if !r.Failed() {
		t.Error("not Failed")
	}
	if it, ok := find(r, "App org.example.App", "org.example.App: not checked"); !ok || it.Level != Warn {
		t.Errorf("app: %+v %v", it, ok)
	}
}
