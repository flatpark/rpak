package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The --extra-data flows end to end are in test/e2e (they need a real
// flatpak to export and deploy); here: usage, the choice of the source,
// that a dry run downloads and writes nothing, and that a failure leaves
// nothing behind.

// extraApp sets up an app with the given [Extra Data] lines in the remote
// upstream of the user installation, as remote-info --show-ref
// --show-commit answers.
func extraApp(e *env, id string, extra ...string) {
	os.MkdirAll(filepath.Join(e.base, "repo"), 0o755)
	os.WriteFile(filepath.Join(e.base, "repo", "config"),
		[]byte("[core]\nrepo_version=1\n\n[remote \"upstream\"]\nurl=file:///nonexistent\n"), 0o644)
	e.write("remotes-user", "upstream\n")
	e.write(filepath.Join("info", "user", "upstream", id),
		"app/"+id+"/x86_64/stable "+strings.Repeat("a", 64)+"\n[Application]\nname="+id+"\nruntime=org.freedesktop.Platform/x86_64/25.08\n\n[Extra Data]\n"+
			strings.Join(extra, "\n")+"\n")
}

// twoSources are two extra-data on one host and in one directory: no URL
// tells them apart.
func twoSources(e *env) []string {
	return []string{
		"name=app.deb", "checksum=" + strings.Repeat("1", 64), "size=10", "uri=" + e.srv.URL + "/dl/app_1.0.deb",
		"name1=helper.deb", "checksum1=" + strings.Repeat("2", 64), "size1=20", "uri1=" + e.srv.URL + "/dl/helper_1.0.deb",
	}
}

func TestExtraDataUsageErrors(t *testing.T) {
	e := newEnv(t)
	extraApp(e, "org.example.App", twoSources(e)...)
	url := e.srv.URL + "/dl/app_2.0.deb"
	for _, argv := range [][]string{
		{"install", "--extra-data=ftp://example.com/x", "upstream", "org.example.App"}, // not http(s)
		{"install", "--extra-data=app.deb", "upstream", "org.example.App"},             // no URL
		{"install", "--extra-data=a=" + url, "--extra-data=a=" + url, "upstream", "org.example.App"},
		{"install", "--extra-data=" + url, "org.example.App"},                         // no REMOTE
		{"install", "--system", "--extra-data=" + url, "upstream", "org.example.App"}, // not the user installation
		{"install", "--extra-data=" + url, "upstream", "org.example.App", "org.example.Other"},
		{"install", "--extra-data=" + url, "--bundle", "x.flatpak"},
		{"update", "--extra-data=" + url}, // which app?
		{"update", "--system", "--extra-data=" + url, "org.example.App"},
		{"patch", "--extra-data"},
		{"patch", "--extra-data=" + url},
		{"patch", "--extra-data=" + url, "--reset", "org.example.App"},
		{"patch", "--extra-data=" + url, "--list"},
		// Two sources, the URL is next to both: say which.
		{"install", "--extra-data=" + url, "upstream", "org.example.App"},
		// Two URLs for one source.
		{"install", "--extra-data=app.deb=" + url, "--extra-data=" + e.srv.URL + "/elsewhere/app.deb", "upstream", "org.example.App"},
		{"install", "--extra-data=nope.deb=" + url, "upstream", "org.example.App"},
	} {
		os.Remove(filepath.Join(e.fake, "argv"))
		out := e.run(argv...)
		if out.code != 2 {
			t.Errorf("%q: exit %d\n%s", argv, out.code, out.stderr)
		}
		if e.read("argv") != "" {
			t.Errorf("%q: flatpak was run", argv)
		}
	}
	if n := e.hits.Load(); n != 0 {
		t.Errorf("%d downloads", n)
	}
	if _, err := os.Stat(filepath.Join(e.base, "rpak", "patches.json")); !os.IsNotExist(err) {
		t.Errorf("patches.json: %v", err)
	}
}

// TestExtraDataAmbiguous: the error lists every source by name and URL,
// for NAME=URL.
func TestExtraDataAmbiguous(t *testing.T) {
	e := newEnv(t)
	extraApp(e, "org.example.App", twoSources(e)...)
	out := e.run("install", "-y", "--extra-data="+e.srv.URL+"/dl/app_2.0.deb", "upstream", "org.example.App")
	if out.code != 2 {
		t.Fatalf("exit %d\n%s", out.code, out.stderr)
	}
	wantLines(t, out.stderr,
		"rpak: org.example.App: cannot tell which extra-data "+e.srv.URL+"/dl/app_2.0.deb replaces; give the source as NAME=URL, one of:",
		"  app.deb  "+e.srv.URL+"/dl/app_1.0.deb",
		"  helper.deb  "+e.srv.URL+"/dl/helper_1.0.deb")
}

func TestExtraDataNeedsExtraData(t *testing.T) {
	e := newEnv(t)
	forceApp(e, "org.example.App", "org.freedesktop.Platform/x86_64/24.08")
	out := e.run("install", "-y", "--extra-data="+e.srv.URL+"/x.deb", "upstream", "org.example.App")
	if out.code != 2 || !strings.Contains(out.stderr, "rpak: org.example.App has no extra-data: --extra-data replaces the download of an extra-data source\n") {
		t.Errorf("exit %d\n%s", out.code, out.stderr)
	}
	if e.read("argv") != "" {
		t.Errorf("flatpak was run")
	}
}

// TestExtraDataDryRun: RPAK_DRY_RUN=1 says what it would download, export,
// install and record, and downloads, writes and runs nothing.
func TestExtraDataDryRun(t *testing.T) {
	e := newEnv(t)
	flatpak := filepath.Join(e.fake, "flatpak")
	extraApp(e, "org.example.App", twoSources(e)...)
	repo := filepath.Join(e.base, "rpak", "local-repo")
	ref := "app/org.example.App/x86_64/stable"
	before := tree(t, e.base)
	newURL := e.srv.URL + "/dl/app_2.0.deb"
	out := e.dryRun("install", "-y", "--force-runtime=26.08", "--extra-data=app.deb="+newURL, "upstream", "org.example.App")
	if out.code != 0 {
		t.Fatalf("exit %d\n%s", out.code, out.stderr)
	}
	commit := strings.Repeat("a", 64)
	wantLines(t, out.stderr,
		"rpak: dry run, would download "+newURL+" as app.deb of org.example.App, instead of "+e.srv.URL+"/dl/app_1.0.deb",
		"rpak: dry run, would reexport org.example.App "+commit[:12]+" from upstream with runtime org.freedesktop.Platform/x86_64/26.08, extra-data app.deb into "+repo,
		"rpak: dry run, would run: "+flatpak+" build-export --arch=x86_64 --no-update-summary"+
			" '--subject=<upstream'\\''s subject> (rpak: runtime org.freedesktop.Platform/x86_64/26.08, extra-data app.deb)'"+
			" $'--body=rpak.upstream="+commit+"\\nrpak.runtime=org.freedesktop.Platform/x86_64/26.08\\nrpak.extra-data=app.deb:<sha256 of app.deb>' "+repo+" '<checkout of "+commit[:12]+">' stable",
		"rpak: dry run, would run: "+flatpak+" install -y --user rpak-local "+ref,
		"rpak: dry run, would record org.example.App in "+filepath.Join(e.base, "rpak", "patches.json")+": extra-data app.deb, "+newURL+" instead of "+e.srv.URL+"/dl/app_1.0.deb",
	)
	// Given NAME: nothing chosen, nothing said about it.
	if strings.Contains(out.stderr, "Using source") || strings.Contains(out.stderr, "experimental") {
		t.Errorf("stderr:\n%s", out.stderr)
	}
	if n := e.hits.Load(); n != 0 {
		t.Errorf("the dry run downloaded (%d requests)", n)
	}
	if after := tree(t, e.base); after != before {
		t.Errorf("the dry run changed files:\n%s\nwas\n%s", after, before)
	}
	for _, call := range strings.Split(strings.TrimSpace(e.read("calls")), "\n") {
		if cmd := strings.Fields(call)[0]; cmd != "remotes" && cmd != "remote-info" {
			t.Errorf("flatpak %s", call)
		}
	}
}

// TestPatchExtraDataDryRun: on an installed app, its only source is taken
// (and said so), the installed commit is reexported and the app
// reinstalled from rpak-local.
func TestPatchExtraDataDryRun(t *testing.T) {
	e := newEnv(t)
	flatpak := filepath.Join(e.fake, "flatpak")
	ref, dir := deployFixture(t, e, "org.rpak.DeployExtra", "extra", false)
	commit := filepath.Base(dir)
	before := tree(t, e.base)
	out := e.dryRun("patch", "-y", "--extra-data", "https://example.org/v2/payload.txt", "org.rpak.DeployExtra")
	if out.code != 0 {
		t.Fatalf("exit %d\n%s", out.code, out.stderr)
	}
	wantLines(t, out.stderr,
		"rpak: Using source payload.txt (https://example.org/payload.txt → https://example.org/v2/payload.txt)",
		"rpak: dry run, would download https://example.org/v2/payload.txt as payload.txt of org.rpak.DeployExtra, instead of https://example.org/payload.txt",
		"rpak: dry run, would reexport org.rpak.DeployExtra "+commit[:12]+" from local with runtime org.freedesktop.Platform/x86_64/25.08, extra-data payload.txt into "+filepath.Join(e.base, "rpak", "local-repo"),
		"rpak: dry run, would run: "+flatpak+" install -y --user --reinstall rpak-local "+ref,
	)
	if after := tree(t, e.base); after != before {
		t.Errorf("the dry run changed files")
	}
	if calls := e.read("calls"); calls != "" {
		t.Errorf("flatpak was run: %s", calls)
	}
}

// TestExtraDataFailures: a download that fails, or an export that fails
// after the download, leaves no record, no file and no flatpak run.
func TestExtraDataFailures(t *testing.T) {
	e := newEnv(t)
	extraApp(e, "org.example.App", "name=app.deb", "checksum="+strings.Repeat("1", 64), "size=10", "uri="+e.srv.URL+"/dl/app_1.0.deb")
	out := e.run("install", "-y", "--extra-data="+e.srv.URL+"/dl/missing.deb", "upstream", "org.example.App")
	if out.code != 1 || !strings.Contains(out.stderr, "404") {
		t.Errorf("404: exit %d\n%s", out.code, out.stderr)
	}
	if !strings.Contains(out.stderr, "rpak: Using source app.deb ("+e.srv.URL+"/dl/app_1.0.deb → "+e.srv.URL+"/dl/missing.deb)\n") {
		t.Errorf("the source taken is not said:\n%s", out.stderr)
	}
	// The download works, the export cannot (the remote is not there).
	content := []byte("the new release")
	e.mu.Lock()
	e.files["/dl/app_2.0.deb"] = content
	e.mu.Unlock()
	out = e.run("install", "-y", "--extra-data="+e.srv.URL+"/dl/app_2.0.deb", "upstream", "org.example.App")
	if out.code != 1 || !strings.Contains(out.stderr, "reexporting") {
		t.Errorf("export: exit %d\n%s", out.code, out.stderr)
	}
	if e.read("argv") != "" {
		t.Errorf("flatpak install was run")
	}
	if _, err := os.Stat(filepath.Join(e.base, "rpak", "patches.json")); !os.IsNotExist(err) {
		t.Errorf("patches.json: %v", err)
	}
	if dirs := extraDataDirs(t, e.base); len(dirs) != 0 {
		t.Errorf("left in extra-data: %v", dirs)
	}
	// It was downloaded, once.
	sum := sha256.Sum256(content)
	if !strings.Contains(out.stderr, "app.deb") || e.hits.Load() != 2 {
		t.Errorf("%d requests (%s)\n%s", e.hits.Load(), hex.EncodeToString(sum[:]), out.stderr)
	}
}

// installedExtra lays out an installed app (deployed by flatpak from the
// remote flathub) with an extra-data record, and, with forced, a
// force-runtime record of method reexport too.
func installedExtra(t *testing.T, e *env, ref string, forced bool) {
	t.Helper()
	installedForced(t, e, ref)
	runtime := ""
	if forced {
		runtime = `"force-runtime":{"target":"26.08","remote":"flathub","method":"reexport","original_runtime":"org.freedesktop.Platform/x86_64/25.08","runtime":"org.freedesktop.Platform/x86_64/26.08","upstream_commit":"` + strings.Repeat("c", 64) + `"},`
	}
	os.WriteFile(filepath.Join(e.base, "rpak", "patches.json"), []byte(`{"version":1,"apps":{"`+ref+`":{`+runtime+`"extra-data":
		{"remote":"flathub","upstream_commit":"`+strings.Repeat("c", 64)+`","sources":[{"name":"app.deb",
		 "url":"https://new.example.com/app_2.deb","sha256":"`+strings.Repeat("e", 64)+`","size":2,
		 "base":{"url":"https://old.example.com/app_1.deb","sha256":"`+strings.Repeat("a", 64)+`","size":1}}]}}}}`), 0o644)
}

func TestPatchListExtraData(t *testing.T) {
	e := newEnv(t)
	installedExtra(t, e, "app/org.example.App/x86_64/stable", true)
	out, err := e.command("patch", "--list").Output()
	want := "App              Runtime\n" +
		"org.example.App  org.freedesktop.Platform 25.08 → 26.08 (pinned)\n" +
		"\n" +
		"org.example.App  app.deb\n" +
		"  url       https://new.example.com/app_2.deb\n" +
		"  upstream  https://old.example.com/app_1.deb\n"
	if err != nil || string(out) != want {
		t.Errorf("%v:\n%s\nwant\n%s", err, out, want)
	}
}

// TestPatchResetExtraData: --reset=KIND undoes that kind only; an app
// left without patches is reinstalled from its remote; asking for a kind
// the app does not have is an error.
func TestPatchResetExtraData(t *testing.T) {
	e := newEnv(t)
	ref := "app/org.example.App/x86_64/stable"
	installedExtra(t, e, ref, false)
	if out := e.run("patch", "--reset=force-runtime", "org.example.App"); out.code != 1 || !strings.Contains(out.stderr, "org.example.App is not patched with force-runtime") {
		t.Errorf("reset=force-runtime: exit %d\n%s", out.code, out.stderr)
	}
	out := e.run("patch", "--reset=extra-data", "-y", "org.example.App")
	if out.code != 0 {
		t.Fatalf("exit %d\n%s", out.code, out.stderr)
	}
	e.assertArgv([]string{"install", "--user", "--reinstall", "-y", "flathub", ref})
	wantLines(t, out.stderr, "rpak: org.example.App comes from flathub again")
	if _, err := os.Stat(filepath.Join(e.base, "rpak", "patches.json")); !os.IsNotExist(err) {
		t.Errorf("the patch is still recorded: %v", err)
	}

	// Both kinds, the app not from rpak-local (the fixture's origin is
	// another remote): resetting one only forgets it, without flatpak.
	installedExtra(t, e, ref, true)
	os.Remove(filepath.Join(e.fake, "argv"))
	if out := e.run("patch", "--reset=extra-data", "-y", "org.example.App"); out.code != 0 {
		t.Fatalf("exit %d\n%s", out.code, out.stderr)
	}
	if e.read("argv") != "" {
		t.Errorf("flatpak was run: %q", e.read("argv"))
	}
	b, _ := os.ReadFile(filepath.Join(e.base, "rpak", "patches.json"))
	if !strings.Contains(string(b), `"force-runtime"`) || strings.Contains(string(b), `"extra-data"`) {
		t.Errorf("patches.json:\n%s", b)
	}
}

// TestUpdateExtraDataDryRun: rpak update --extra-data takes upstream's
// latest commit of the installed app, and moves it to rpak-local instead
// of updating it from its remote.
func TestUpdateExtraDataDryRun(t *testing.T) {
	e := newEnv(t)
	flatpak := filepath.Join(e.fake, "flatpak")
	ref, _ := deployFixture(t, e, "org.rpak.DeployExtra", "extra", false)
	e.write("remotes-user", "local\n")
	latest := strings.Repeat("d", 64)
	e.write(filepath.Join("info", "user", "local", strings.ReplaceAll(ref, "/", "_")),
		ref+" "+latest+"\n[Application]\nname=org.rpak.DeployExtra\nruntime=org.freedesktop.Platform/x86_64/25.08\n\n[Extra Data]\nname=payload.txt\nchecksum="+
			strings.Repeat("0", 63)+"2\nsize=11\nuri=https://example.org/payload-2.txt\n")
	before := tree(t, e.base)
	out := e.dryRun("update", "-y", "--extra-data=https://example.org/beta/payload.txt", "org.rpak.DeployExtra")
	if out.code != 0 {
		t.Fatalf("exit %d\n%s", out.code, out.stderr)
	}
	wantLines(t, out.stderr,
		"rpak: Using source payload.txt (https://example.org/payload-2.txt → https://example.org/beta/payload.txt)",
		"rpak: dry run, would reexport org.rpak.DeployExtra "+latest[:12]+" from local with runtime org.freedesktop.Platform/x86_64/25.08, extra-data payload.txt into "+filepath.Join(e.base, "rpak", "local-repo"),
		"rpak: dry run, would run: "+flatpak+" install -y --reinstall rpak-local "+ref,
		"rpak: dry run, would record org.rpak.DeployExtra in "+filepath.Join(e.base, "rpak", "patches.json")+": extra-data payload.txt, https://example.org/beta/payload.txt instead of https://example.org/payload-2.txt",
	)
	// Not updated from its remote first: it moves to rpak-local.
	if strings.Contains(out.stderr, "would run: "+flatpak+" update") {
		t.Errorf("an update from upstream:\n%s", out.stderr)
	}
	if after := tree(t, e.base); after != before {
		t.Errorf("the dry run changed files")
	}
}
