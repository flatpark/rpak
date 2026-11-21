package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	fr "github.com/flatpark/rpak/internal/forceruntime"
	"github.com/flatpark/rpak/internal/gvariant"
)

// The --force-runtime flows end to end are in test/e2e (they need a real
// flatpak to deploy); here: what flatpak is asked to do, and that nothing
// changes without the option and without records.

// TestNoRecordsNoExtraCalls: without --force-runtime and without records,
// install and update run exactly the queries they ran before forcing
// existed, and leave no trace of it.
func TestNoRecordsNoExtraCalls(t *testing.T) {
	e := newEnv(t)
	e.write("remotes-user", "flatpark\n")
	e.app("user", "flatpark", "app/com.example.One/x86_64/stable", "com.example.One", 1000)
	argv := []string{"install", "--user", "-y", "flatpark", "com.example.One"}
	if out := e.run(argv...); out.code != 0 {
		t.Fatalf("exit %d\n%s", out.code, out.stderr)
	}
	e.assertArgv(argv)
	want := "remotes --user --columns=name\n" +
		"remote-info --user --show-ref --show-metadata flatpark com.example.One\n" +
		strings.Join(argv, " ") + "\n"
	if got := e.read("calls"); got != want {
		t.Errorf("calls:\n%s\nwant\n%s", got, want)
	}
	os.Remove(filepath.Join(e.fake, "calls"))
	if out := e.run("update", "-y"); out.code != 0 {
		t.Fatalf("update: exit %d\n%s", out.code, out.stderr)
	}
	for _, call := range strings.Split(strings.TrimSpace(e.read("calls")), "\n") {
		cmd := strings.Fields(call)[0]
		if cmd != "remote-ls" && cmd != "remotes" && cmd != "remote-info" && cmd != "update" {
			t.Errorf("update ran flatpak %s", call)
		}
	}
	if _, err := os.Stat(filepath.Join(e.base, "rpak")); !os.IsNotExist(err) {
		t.Errorf("rpak/ was created in the user installation: %v", err)
	}
}

// forceApp sets up an app without extra-data in the remote upstream of the
// user installation, as remote-info --show-ref --show-commit answers.
func forceApp(e *env, id, runtime string) {
	os.MkdirAll(filepath.Join(e.base, "repo"), 0o755)
	os.WriteFile(filepath.Join(e.base, "repo", "config"),
		[]byte("[core]\nrepo_version=1\n\n[remote \"upstream\"]\nurl=file:///nonexistent\n"), 0o644)
	e.write("remotes-user", "upstream\n")
	e.write(filepath.Join("info", "user", "upstream", id),
		"app/"+id+"/x86_64/stable "+strings.Repeat("a", 64)+"\n[Application]\nname="+id+"\nruntime="+runtime+"\n")
}

func TestForceInstallArgv(t *testing.T) {
	e := newEnv(t)
	forceApp(e, "org.example.App", "org.freedesktop.Platform/x86_64/24.08")
	out := e.run("install", "-y", "--force-runtime=25.08", "upstream", "org.example.App")
	if out.code != 0 {
		t.Fatalf("exit %d\n%s", out.code, out.stderr)
	}
	// The option is rpak's; the remote is only the user installation's, so
	// --user is said; --no-deps keeps flatpak from pulling 24.08; the app
	// is named by the ref remote-info resolved.
	e.assertArgv([]string{"install", "-y", "--user", "--no-deps", "upstream", "app/org.example.App/x86_64/stable"})
	if !strings.Contains(e.read("calls"), "remote-info --user --show-ref --show-commit --show-metadata upstream org.example.App\n") {
		t.Errorf("no forcing query:\n%s", e.read("calls"))
	}
	// Not deployed by the fake flatpak: nothing to patch, no warning.
	if strings.Contains(out.stderr, "experimental") {
		t.Errorf("stderr:\n%s", out.stderr)
	}
}

func TestForceInstallUpstreamAlreadyThere(t *testing.T) {
	e := newEnv(t)
	forceApp(e, "org.example.App", "org.freedesktop.Platform/x86_64/25.08")
	out := e.run("install", "-y", "--force-runtime=25.08", "upstream", "org.example.App")
	if out.code != 0 {
		t.Fatalf("exit %d\n%s", out.code, out.stderr)
	}
	if !strings.Contains(out.stderr, "upstream is already on org.freedesktop.Platform/x86_64/25.08, not forcing") {
		t.Errorf("stderr:\n%s", out.stderr)
	}
	// Installed as asked, with its dependencies.
	e.assertArgv([]string{"install", "-y", "--user", "upstream", "org.example.App"})
}

func TestForceUsageErrors(t *testing.T) {
	e := newEnv(t)
	forceApp(e, "org.example.App", "org.freedesktop.Platform/x86_64/24.08")
	for _, argv := range [][]string{
		{"install", "--force-runtime", "org.example.App"},                         // no REMOTE
		{"install", "--system", "--force-runtime", "upstream", "org.example.App"}, // not the user installation
		{"install", "--force-runtime=a/b", "upstream", "org.example.App"},         // bad target
		{"install", "--force-runtime", "--bundle", "x.flatpak"},
		{"update", "--force-runtime"}, // all apps at once
		{"update", "--system", "--force-runtime", "org.example.App"},
		{"patch", "--reset"},                                       // which apps?
		{"patch", "--force-runtime"},                               // which apps?
		{"patch", "--reset=other", "org.example.App"},              // no such kind
		{"patch", "--reset", "--force-runtime", "org.example.App"}, // one at a time
		{"patch", "--system", "--force-runtime", "org.example.App"},
		{"patch", "--frobnicate"},
		{"patch", "org.example.App"},
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
}

func TestBrokenRecordsAreSetAside(t *testing.T) {
	e := newEnv(t)
	dir := filepath.Join(e.base, "rpak")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "patches.json"), []byte("{not json"), 0o644)
	out := e.run("update", "-y")
	if out.code != 0 {
		t.Fatalf("exit %d\n%s", out.code, out.stderr)
	}
	e.assertArgv([]string{"update", "-y"})
	if !strings.Contains(out.stderr, "patches.json.broken") {
		t.Errorf("no warning:\n%s", out.stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "patches.json")); !os.IsNotExist(err) {
		t.Errorf("the broken file is still in place")
	}
	// The next run is back to no records.
	if out := e.run("update", "-y"); out.code != 0 || strings.Contains(out.stderr, "warning") {
		t.Errorf("second run: exit %d\n%s", out.code, out.stderr)
	}
}

func TestPatchListEmpty(t *testing.T) {
	e := newEnv(t)
	for _, argv := range [][]string{{"patch"}, {"patch", "--list"}} {
		out, err := e.command(argv...).Output()
		if err != nil || string(out) != "No app is patched.\n" {
			t.Errorf("%q: %v: %s", argv, err, out)
		}
	}
}

func TestPatchList(t *testing.T) {
	e := newEnv(t)
	installedForced(t, e, "app/org.example.App/x86_64/stable")
	out, err := e.command("patch").Output()
	want := "App              Runtime\n" +
		"org.example.App  org.freedesktop.Platform 25.08 → 26.08 (pinned)\n"
	if err != nil || string(out) != want {
		t.Errorf("%v:\n%s\nwant\n%s", err, out, want)
	}
}

// TestPatchResetArgv: --reset reinstalls from the upstream remote, and
// only once that worked forgets the patch.
func TestPatchResetArgv(t *testing.T) {
	e := newEnv(t)
	installedForced(t, e, "app/org.example.App/x86_64/stable")
	e.write("mode", "exit 1")
	out := e.run("patch", "--reset", "-y", "org.example.App")
	if out.code != 1 {
		t.Fatalf("exit %d\n%s", out.code, out.stderr)
	}
	e.assertArgv([]string{"install", "--user", "--reinstall", "-y", "flathub", "app/org.example.App/x86_64/stable"})
	if _, err := os.Stat(filepath.Join(e.base, "rpak", "patches.json")); err != nil {
		t.Fatalf("a failed reinstall forgot the patch: %v", err)
	}
	os.Remove(filepath.Join(e.fake, "mode"))
	if out := e.run("patch", "--reset=force-runtime", "-y", "org.example.App"); out.code != 0 {
		t.Fatalf("exit %d\n%s", out.code, out.stderr)
	}
	if _, err := os.Stat(filepath.Join(e.base, "rpak", "patches.json")); !os.IsNotExist(err) {
		t.Errorf("the patch is still recorded")
	}
	if out := e.run("patch", "--reset", "org.example.App"); out.code != 1 || !strings.Contains(out.stderr, "is not patched") {
		t.Errorf("reset again: exit %d\n%s", out.code, out.stderr)
	}
}

// installedForced lays out an installed app with a patch record in the
// user installation of e.
func installedForced(t *testing.T, e *env, ref string) {
	t.Helper()
	dir := filepath.Join(e.base, filepath.FromSlash(ref), strings.Repeat("c", 64))
	os.MkdirAll(dir, 0o755)
	deploy, err := os.ReadFile("../../internal/gvariant/testdata/real.deploy")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "deploy"), deploy, 0o644)
	os.WriteFile(filepath.Join(dir, "metadata"), []byte("[Application]\nname=x\nruntime=org.freedesktop.Platform/x86_64/26.08\n"), 0o644)
	os.Symlink(strings.Repeat("c", 64), filepath.Join(e.base, filepath.FromSlash(ref), "active"))
	os.MkdirAll(filepath.Join(e.base, "rpak"), 0o755)
	os.WriteFile(filepath.Join(e.base, "rpak", "patches.json"), []byte(`{"version":1,"apps":{"`+ref+`":{"force-runtime":
		{"target":"26.08","remote":"flathub","method":"patch",
		 "original_runtime":"org.freedesktop.Platform/x86_64/25.08","runtime":"org.freedesktop.Platform/x86_64/26.08"}}}}`), 0o644)
}

// TestUpdateWithPatchRecordSplits: --no-deps keeps flatpak from pulling
// the old runtime of a patched app, but must not keep the system
// installation's apps from the runtimes they move to.
func TestUpdateWithPatchRecordSplits(t *testing.T) {
	e := newEnv(t)
	installedForced(t, e, "app/org.example.App/x86_64/stable")
	out := e.run("update", "-y")
	if out.code != 0 {
		t.Fatalf("exit %d\n%s", out.code, out.stderr)
	}
	var updates []string
	for _, call := range strings.Split(strings.TrimSpace(e.read("calls")), "\n") {
		if strings.HasPrefix(call, "update") {
			updates = append(updates, call)
		}
	}
	want := []string{"update -y --user --no-deps", "update -y --system"}
	if strings.Join(updates, "\n") != strings.Join(want, "\n") {
		t.Errorf("updates:\n%s\nwant\n%s\nstderr:\n%s", strings.Join(updates, "\n"), strings.Join(want, "\n"), out.stderr)
	}

	// Only the user installation: one call, with --no-deps.
	os.Remove(filepath.Join(e.fake, "calls"))
	if out := e.run("update", "--user", "-y", "org.example.App"); out.code != 0 {
		t.Fatalf("exit %d\n%s", out.code, out.stderr)
	}
	calls := e.read("calls")
	if !strings.Contains(calls, "\nupdate --user -y --no-deps org.example.App\n") && !strings.HasPrefix(calls, "update --user -y --no-deps org.example.App\n") {
		t.Errorf("calls:\n%s", calls)
	}
	// The runtime the patched app names is installed nowhere: rpak
	// installs it, as --no-deps left it out.
	if !strings.Contains(calls, "install --user -y --noninteractive runtime/org.freedesktop.Platform/x86_64/26.08\n") {
		t.Errorf("no runtime install:\n%s", calls)
	}

	// The system installation alone: nothing forced, as given.
	if out := e.run("update", "--system", "-y"); out.code != 0 {
		t.Fatalf("exit %d\n%s", out.code, out.stderr)
	}
	e.assertArgv([]string{"update", "--system", "-y"})
}

// TestStaleRecordIsDropped: an app uninstalled with flatpak is forgotten
// by the next rpak run, silently, and nothing is forced.
func TestStaleRecordIsDropped(t *testing.T) {
	e := newEnv(t)
	installedForced(t, e, "app/org.example.App/x86_64/stable")
	os.RemoveAll(filepath.Join(e.base, "app"))
	out := e.run("update", "-y")
	if out.code != 0 || strings.Contains(out.stderr, "warning") {
		t.Fatalf("exit %d\n%s", out.code, out.stderr)
	}
	e.assertArgv([]string{"update", "-y"})
	if _, err := os.Stat(filepath.Join(e.base, "rpak", "patches.json")); !os.IsNotExist(err) {
		t.Errorf("the records file is still there")
	}
}

// deployFixture installs an app the way flatpak leaves it, from a commit
// made by flatpak build-export (internal/forceruntime/testdata/gen.sh):
// the commit in the repository, the deployment with the commit's metadata
// (or forced to 26.08) and a deploy file of origin "local", a remote of
// the user installation.
func deployFixture(t *testing.T, e *env, id, fixture string, forced bool) (ref, dir string) {
	t.Helper()
	deploy := "real.deploy"
	if forced {
		deploy = "real.forced.deploy"
	}
	data, err := os.ReadFile("../../internal/gvariant/testdata/" + deploy)
	if err != nil {
		t.Fatal(err)
	}
	dd, err := gvariant.ParseDeployData(data)
	if err != nil {
		t.Fatal(err)
	}
	// The deploy file names the commit; the fixture's commit object is
	// stored under that name (nothing here checks object checksums).
	commit := dd.Commit
	obj, err := os.ReadFile("../../internal/forceruntime/testdata/" + fixture + ".commit")
	if err != nil {
		t.Fatal(err)
	}
	objPath := filepath.Join(e.base, "repo", "objects", commit[:2], commit[2:]+".commit")
	os.MkdirAll(filepath.Dir(objPath), 0o755)
	os.WriteFile(objPath, obj, 0o644)
	os.WriteFile(filepath.Join(e.base, "repo", "config"),
		[]byte("[core]\nrepo_version=1\n\n[remote \"local\"]\nurl=file:///nonexistent\n"), 0o644)
	text, err := fr.CommitMetadata(e.base, commit)
	if err != nil {
		t.Fatal(err)
	}
	if forced {
		from, _ := fr.ParsePref("org.freedesktop.Platform/x86_64/25.08")
		to, _ := fr.ParsePref("org.freedesktop.Platform/x86_64/26.08")
		if text, err = fr.RewriteMetadata(text, from, to); err != nil {
			t.Fatal(err)
		}
	}
	ref = "app/" + id + "/x86_64/stable"
	dir = filepath.Join(e.base, filepath.FromSlash(ref), commit)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "metadata"), []byte(text), 0o644)
	os.WriteFile(filepath.Join(dir, "deploy"), data, 0o644)
	os.Symlink(commit, filepath.Join(e.base, filepath.FromSlash(ref), "active"))
	return ref, dir
}

// tree lists every file below dir with its contents' checksum.
func tree(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, _ := d.Info()
		fmt.Fprintf(&b, "%s %v", p, info.Mode())
		if info.Mode().IsRegular() {
			data, _ := os.ReadFile(p)
			fmt.Fprintf(&b, " %x", sha256.Sum256(data))
		}
		b.WriteByte('\n')
		return nil
	})
	return b.String()
}

func (e *env) dryRun(argv ...string) outcome {
	e.t.Helper()
	cmd := e.command(argv...)
	cmd.Env = append(cmd.Env, "RPAK_DRY_RUN=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	code := 0
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		e.t.Fatal(err)
	}
	return outcome{code, stderr.String()}
}

func wantLines(t *testing.T, stderr string, lines ...string) {
	t.Helper()
	for _, l := range lines {
		if !strings.Contains(stderr, l+"\n") {
			t.Errorf("stderr lacks %q:\n%s", l, stderr)
		}
	}
}

// TestPatchDryRun: rpak patch --force-runtime and --reset under
// RPAK_DRY_RUN=1 say what they would record and do, and touch nothing.
func TestPatchDryRun(t *testing.T) {
	e := newEnv(t)
	flatpak := filepath.Join(e.fake, "flatpak")
	ref, dir := deployFixture(t, e, "org.rpak.Deploy", "app", false)
	before := tree(t, e.base)
	out := e.dryRun("patch", "--force-runtime=26.08", "org.rpak.Deploy")
	if out.code != 0 {
		t.Fatalf("exit %d\n%s", out.code, out.stderr)
	}
	wantLines(t, out.stderr,
		"rpak: dry run, would rewrite metadata and deploy of org.rpak.Deploy ("+dir+"): runtime org.freedesktop.Platform/x86_64/25.08 -> org.freedesktop.Platform/x86_64/26.08",
		"rpak: dry run, would record org.rpak.Deploy in "+filepath.Join(e.base, "rpak", "patches.json")+": force-runtime, target 26.08, method patch, org.freedesktop.Platform/x86_64/25.08 -> org.freedesktop.Platform/x86_64/26.08",
		"rpak: dry run, would run: "+flatpak+" install --user -y --noninteractive runtime/org.freedesktop.Platform/x86_64/26.08",
	)
	if strings.Contains(out.stderr, "experimental") {
		t.Errorf("a dry run announced:\n%s", out.stderr)
	}
	if after := tree(t, e.base); after != before {
		t.Errorf("the dry run changed files:\n%s\nwas\n%s", after, before)
	}
	if calls := e.read("calls"); calls != "" {
		t.Errorf("flatpak was run: %s", calls)
	}

	// Patched for real, then a dry run of --reset.
	if out := e.run("patch", "--force-runtime=26.08", "org.rpak.Deploy"); out.code != 0 {
		t.Fatalf("exit %d\n%s", out.code, out.stderr)
	}
	os.Remove(filepath.Join(e.fake, "calls"))
	before = tree(t, e.base)
	out = e.dryRun("patch", "--reset", "-y", "org.rpak.Deploy")
	if out.code != 0 {
		t.Fatalf("exit %d\n%s", out.code, out.stderr)
	}
	wantLines(t, out.stderr,
		"rpak: dry run, would run: "+flatpak+" install --user --reinstall -y local "+ref,
		"rpak: dry run, would make org.rpak.Deploy run on org.freedesktop.Platform/x86_64/25.08 again",
		"rpak: dry run, would forget the force-runtime patch of org.rpak.Deploy in "+filepath.Join(e.base, "rpak", "patches.json"),
	)
	if after := tree(t, e.base); after != before {
		t.Errorf("the dry run of --reset changed files")
	}
	if calls := e.read("calls"); calls != "" {
		t.Errorf("flatpak was run: %s", calls)
	}
}

// TestPatchDryRunReexport: for an app with extra-data the dry run shows
// the export into rpak-local and the flatpak commands, and makes neither
// the repository nor the remote.
func TestPatchDryRunReexport(t *testing.T) {
	e := newEnv(t)
	flatpak := filepath.Join(e.fake, "flatpak")
	ref, dir := deployFixture(t, e, "org.rpak.DeployExtra", "extra", false)
	commit := filepath.Base(dir)
	repo := filepath.Join(e.base, "rpak", "local-repo")
	before := tree(t, e.base)
	out := e.dryRun("patch", "-y", "--force-runtime=26.08", "org.rpak.DeployExtra")
	if out.code != 0 {
		t.Fatalf("exit %d\n%s", out.code, out.stderr)
	}
	wantLines(t, out.stderr,
		"rpak: dry run, would reexport org.rpak.DeployExtra "+commit[:12]+" from local with runtime org.freedesktop.Platform/x86_64/26.08 into "+repo,
		"rpak: dry run, would run: "+flatpak+" build-update-repo --no-update-appstream --prune --prune-depth=0 "+repo,
		"rpak: dry run, would run: "+flatpak+" remote-add --user --no-gpg-verify --no-enumerate --if-not-exists rpak-local file://"+repo,
		"rpak: dry run, would run: "+flatpak+" install -y --user --reinstall rpak-local "+ref,
		"rpak: dry run, would record org.rpak.DeployExtra in "+filepath.Join(e.base, "rpak", "patches.json")+": force-runtime, target 26.08, method reexport, org.freedesktop.Platform/x86_64/25.08 -> org.freedesktop.Platform/x86_64/26.08",
	)
	// One line, the body in $'...'.
	wantLines(t, out.stderr, "rpak: dry run, would run: "+flatpak+" build-export --arch=x86_64 --no-update-summary"+
		" '--subject=<upstream'\\''s subject> (rpak: runtime org.freedesktop.Platform/x86_64/26.08)'"+
		" $'--body=rpak.upstream="+commit+"\\nrpak.runtime=org.freedesktop.Platform/x86_64/26.08' "+repo+" '<checkout of "+commit[:12]+">' stable")
	// flatpak install from rpak-local brings the runtime itself.
	if strings.Contains(out.stderr, "which org.rpak.DeployExtra needs") {
		t.Errorf("a runtime to install for the reexported app:\n%s", out.stderr)
	}
	if after := tree(t, e.base); after != before {
		t.Errorf("the dry run changed files")
	}
	if calls := e.read("calls"); calls != "" {
		t.Errorf("flatpak was run: %s", calls)
	}
}

// TestNewRecordIsAnnounced: a new record, or a new target, prints the
// experimental line even if the deployment is on the target already; the
// same call again prints nothing.
func TestNewRecordIsAnnounced(t *testing.T) {
	e := newEnv(t)
	deployFixture(t, e, "org.rpak.Deploy", "app", true)
	line := "rpak: experimental: org.rpak.Deploy was built for org.freedesktop.Platform/x86_64/25.08, now runs on org.freedesktop.Platform/x86_64/26.08; it may break. Undo: rpak patch --reset org.rpak.Deploy\n"
	for i, c := range []struct {
		target   string
		announce bool
	}{
		{"26.08", true},  // new record
		{"26.08", false}, // unchanged
		{"org.freedesktop.Platform/x86_64/26.08", true}, // another target
		{"org.freedesktop.Platform/x86_64/26.08", false},
	} {
		out := e.run("patch", "--force-runtime="+c.target, "org.rpak.Deploy")
		if out.code != 0 {
			t.Fatalf("%d: exit %d\n%s", i, out.code, out.stderr)
		}
		if got := strings.Count(out.stderr, line); got != map[bool]int{true: 1, false: 0}[c.announce] {
			t.Errorf("%d (%s): announced %d times:\n%s", i, c.target, got, out.stderr)
		}
	}
	if b, err := os.ReadFile(filepath.Join(e.base, "rpak", "patches.json")); err != nil || !strings.Contains(string(b), `"target": "org.freedesktop.Platform/x86_64/26.08"`) {
		t.Errorf("patches.json: %s %v", b, err)
	}
}
