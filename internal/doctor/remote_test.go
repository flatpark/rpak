package doctor

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flatpark/rpak/internal/summary"
)

// repo is the repository of internal/summary/testdata.
const repo = "../summary/testdata/repo"

// remoteServer serves repo, with changes: files maps paths to other
// contents ("" for 404).
func remoteServer(t *testing.T, files map[string]string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if content, ok := files[r.URL.Path]; ok {
			if content == "" {
				http.NotFound(w, r)
				return
			}
			w.Write([]byte(content))
			return
		}
		http.ServeFile(w, r, filepath.Join(repo, r.URL.Path))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fetchExtra(t *testing.T, url string) ([]summary.Ref, error) {
	t.Helper()
	data, err := FetchSummary(context.Background(), http.DefaultClient, "test", url, "x86_64", "")
	if err != nil {
		return nil, err
	}
	return summary.Find(data, "app/org.example.Extra/x86_64/stable")
}

func TestFetchSummary(t *testing.T) {
	abs, _ := filepath.Abs(repo)
	for name, url := range map[string]string{
		"indexed":                 remoteServer(t, nil).URL + "/",
		"no summary.idx: summary": remoteServer(t, map[string]string{"/summary.idx": ""}).URL,
		"file:":                   "file://" + abs,
		"file: with a trailing /": "file://" + abs + "/",
	} {
		refs, err := fetchExtra(t, url)
		if err != nil || len(refs) != 1 || !strings.Contains(refs[0].Metadata, "uri=https://example.org/app-1.0.deb") {
			t.Errorf("%s: %+v %v", name, refs, err)
		}
	}

	// What fails, and says why.
	big := remoteServer(t, map[string]string{"/summary.idx": "", "/summary": strings.Repeat("x", 2000)})
	var bomb bytes.Buffer
	zw := gzip.NewWriter(&bomb)
	zw.Write(make([]byte, 5000))
	zw.Close()
	subs, _ := summary.Subsummaries(must(os.ReadFile(filepath.Join(repo, "summary.idx"))))
	gz := "/summaries/" + subs["x86_64"] + ".gz"
	for _, c := range []struct {
		name, url, err string
	}{
		{"too big", big.URL, "larger than 1.0 kB"},
		{"decompresses to too much", remoteServer(t, map[string]string{gz: bomb.String()}).URL, ".gz: decompressing: larger than 1.0 kB"},
		{"not gzip", remoteServer(t, map[string]string{gz: "plain"}).URL, ".gz: decompressing: "},
		{"wrong digest", remoteServer(t, map[string]string{gz: gzipped(t, "other")}).URL, "subsummary checksum is"},
		{"no subsummary", remoteServer(t, map[string]string{gz: ""}).URL, "summaries/" + subs["x86_64"] + ".gz: not found"},
		{"neither file", remoteServer(t, map[string]string{"/summary.idx": "", "/summary": ""}).URL, "summary: not found"},
		{"server error", remoteServer(t, map[string]string{"/summary.idx": "x"}).URL, "summary.idx"},
		{"oci", "oci+https://example.org", "not supported"},
	} {
		saved := maxSummary
		maxSummary = 1000
		_, err := fetchExtra(t, c.url)
		maxSummary = saved
		if err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("%s: %v, want %q", c.name, err, c.err)
		}
	}
	if _, err := FetchSummary(context.Background(), http.DefaultClient, "test", remoteServer(t, nil).URL, "aarch64", ""); err == nil || !strings.Contains(err.Error(), "no subsummary aarch64") {
		t.Errorf("other arch: %v", err)
	}
	if _, err := FetchSummary(context.Background(), http.DefaultClient, "test", remoteServer(t, nil).URL, "x86_64", "verified"); err == nil || !strings.Contains(err.Error(), "no subsummary verified-x86_64") {
		t.Errorf("subset: %v", err)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func gzipped(t *testing.T, s string) string {
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	zw.Write([]byte(s))
	zw.Close()
	return b.String()
}

// appDoctor is a doctor for the app checks alone, with the remote flatpark
// of the user installation at srv.
func appDoctor(t *testing.T, srv *httptest.Server, out map[string]string) (*doctor, *instInfo) {
	home := t.TempDir()
	d := &doctor{ctx: context.Background(), flatpak: true, cfg: Config{
		Version: "test", Runner: &fakeRunner{out: out}, Client: toServer(srv), Home: home, Arch: "x86_64",
		LookupEnv: func(string) (string, bool) { return "", false }, NetTimeout: 5 * time.Second, SummaryTimeout: 5 * time.Second,
	}}
	in := &instInfo{exists: true, remotes: []Remote{{Name: "flatpark", URL: srv.URL + "/repo/"}}}
	in.inst.ID, in.inst.User, in.inst.Path = "user", true, filepath.Join(home, ".local/share/flatpak")
	return d, in
}

func labels(checks []commitCheck) []string {
	var out []string
	for _, c := range checks {
		out = append(out, c.label)
	}
	return out
}

func TestAppCommits(t *testing.T) {
	srv := extraDataServer(t, map[string]int{"/app-1.0.deb": 1000, "/app-2.0.deb": 2000, "/old.deb": 10})
	refs, _ := summary.Find(must(os.ReadFile(filepath.Join(repo, "summary"))), "app/org.example.Extra/x86_64/stable")
	remoteCommit := refs[0].Commit
	info := func(commit string) string {
		return "Ref: app/org.example.Extra/x86_64/stable\nOrigin: flatpark\nCommit: " + commit + "\n"
	}
	old := "[Extra Data]\nname=old.deb\nchecksum=" + strings.Repeat("c", 64) + "\nsize=10\nuri=https://example.org/old.deb\n"

	// The installed commit is the remote's: checked once.
	d, in := appDoctor(t, srv, map[string]string{"info --user --show-metadata -- org.example.Extra": refs[0].Metadata})
	ai := &appInst{in: in, info: ParseInfo(info(remoteCommit))}
	d.appDetails("org.example.Extra", ai)
	if got := labels(ai.checks); len(got) != 1 || got[0] != "installed and remote commit" || ai.remoteErr != nil {
		t.Errorf("same commit: %q, %v", got, ai.remoteErr)
	}
	if level, what := ai.checks[0].extra[0].Judge(); level != OK || what != "HTTP 200, 1.0 kB as pinned" {
		t.Errorf("same commit: %v %s", level, what)
	}

	// An update is pending: both, the remote's first.
	d, in = appDoctor(t, srv, map[string]string{"info --user --show-metadata -- org.example.Extra": old})
	ai = &appInst{in: in, info: ParseInfo(info(strings.Repeat("d", 64)))}
	d.appDetails("org.example.Extra", ai)
	if got := strings.Join(labels(ai.checks), "|"); got != "remote commit, pending update|installed commit" {
		t.Errorf("pending update: %s", got)
	}
	if ai.checks[1].extra[0].Source.Name != "old.deb" {
		t.Errorf("installed commit: %+v", ai.checks[1])
	}

	// The summary cannot be read: the installed commit, and why not.
	saved := maxSummary
	maxSummary = 100
	d, in = appDoctor(t, srv, map[string]string{"info --user --show-metadata -- org.example.Extra": old})
	ai = &appInst{in: in, info: ParseInfo(info(remoteCommit))}
	d.appDetails("org.example.Extra", ai)
	maxSummary = saved
	if got := labels(ai.checks); len(got) != 1 || got[0] != "installed commit" || ai.remoteErr == nil || ai.remoteErr.Error() != "cannot read the summary of remote flatpark: summary.idx: larger than 100 bytes" {
		t.Errorf("summary too big: %q, %v", got, ai.remoteErr)
	}

	// The remote no longer has the ref, or is not configured.
	for _, c := range []struct{ ref, origin, err string }{
		{"app/org.example.Gone/x86_64/stable", "flatpark", "remote flatpark no longer has app/org.example.Gone/x86_64/stable"},
		{"app/org.example.Extra/x86_64/stable", "elsewhere", "its remote elsewhere is not configured"},
	} {
		d, in = appDoctor(t, srv, map[string]string{"info --user --show-metadata -- org.example.Extra": old})
		ai = &appInst{in: in, info: ParseInfo("Ref: " + c.ref + "\nOrigin: " + c.origin + "\nCommit: abc\n")}
		d.appDetails("org.example.Extra", ai)
		if ai.remoteErr == nil || ai.remoteErr.Error() != c.err || len(ai.checks) != 1 {
			t.Errorf("%s: %v %q", c.origin, ai.remoteErr, labels(ai.checks))
		}
	}

	// Not installed: every branch in every remote, once per URL.
	d, in = appDoctor(t, srv, nil)
	in2 := &instInfo{exists: true, remotes: []Remote{{Name: "flatpark", URL: srv.URL + "/repo/"}, {Name: "local", URL: "file:///nowhere"}}}
	in2.inst.ID = "default"
	a := &appInfo{id: "org.example.Extra"}
	d.searchRemotes(a, []*instInfo{in, in2})
	var got []string
	for _, f := range a.found {
		level, what := f.check.extra[0].Judge()
		got = append(got, f.check.label+": "+what)
		if level != OK {
			t.Errorf("%s: %v", f.check.label, level)
		}
	}
	if strings.Join(got, "|") != "remote flatpark (User installation), branch beta: HTTP 200, 2.0 kB as pinned|remote flatpark (User installation), branch stable: HTTP 200, 1.0 kB as pinned" || len(a.unreadable) != 0 {
		t.Errorf("not installed: %q, unreadable %q", got, a.unreadable)
	}
	a = &appInfo{id: "org.example.Nope"}
	d.searchRemotes(a, []*instInfo{in})
	if len(a.found) != 0 || len(a.unreadable) != 0 {
		t.Errorf("not anywhere: %+v", a)
	}
}
