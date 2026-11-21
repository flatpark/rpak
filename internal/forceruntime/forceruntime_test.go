package forceruntime

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func pref(s string) Pref {
	p, err := ParsePref(s)
	if err != nil {
		panic(err)
	}
	return p
}

func TestLatestBranch(t *testing.T) {
	for _, tc := range []struct {
		old        string
		candidates []string
		want       string
		ok         bool
	}{
		{"25.08", []string{"24.08", "25.08", "26.08", "25.08beta", "49", "5.15-25.08"}, "26.08", true},
		// numbers, not text: 25.10 > 25.08, 9 < 10
		{"25.08", []string{"25.10", "25.9"}, "25.10", true},
		{"9", []string{"10", "48", "49", "master", "6.9"}, "49", true},
		// KDE 5.15 never moves to 6.x, nor 6.x to 5.15
		{"5.15-24.08", []string{"6.9", "6.10", "5.15-25.08", "5.15-23.08"}, "5.15-25.08", true},
		{"6.8", []string{"5.15-25.08", "6.9", "6.10"}, "6.10", true},
		// nothing newer: the old one
		{"26.08", []string{"25.08", "24.08"}, "26.08", true},
		{"stable", []string{"26.08"}, "", false},
		{"5.15", []string{"5.16"}, "5.16", true}, // a plain N.N
	} {
		got, ok := LatestBranch(tc.old, tc.candidates)
		if got != tc.want || ok != tc.ok {
			t.Errorf("LatestBranch(%s, %v) = %q %v, want %q %v", tc.old, tc.candidates, got, ok, tc.want, tc.ok)
		}
	}
}

func TestResolve(t *testing.T) {
	orig := pref("org.freedesktop.Platform/x86_64/25.08")
	branches := []string{"24.08", "25.08", "26.08"}
	for _, tc := range []struct {
		target string
		want   string
		forced bool
		err    bool
	}{
		{"latest", "org.freedesktop.Platform/x86_64/26.08", true, false},
		{"", "org.freedesktop.Platform/x86_64/26.08", true, false},
		{"26.08", "org.freedesktop.Platform/x86_64/26.08", true, false},
		// only upgrades
		{"25.08", "org.freedesktop.Platform/x86_64/25.08", false, false},
		{"24.08", "org.freedesktop.Platform/x86_64/25.08", false, false},
		{"org.freedesktop.Platform/x86_64/24.08", "org.freedesktop.Platform/x86_64/25.08", false, false},
		{"org.freedesktop.Platform/x86_64/27.08", "org.freedesktop.Platform/x86_64/27.08", true, false},
		{"runtime/org.freedesktop.Platform/x86_64/27.08", "org.freedesktop.Platform/x86_64/27.08", true, false},
		{"org.freedesktop.Platform//27.08", "org.freedesktop.Platform/x86_64/27.08", true, false},
		// another runtime: the user's call, not compared
		{"org.gnome.Platform/x86_64/49", "org.gnome.Platform/x86_64/49", true, false},
		// a branch of another form cannot be compared: taken as asked
		{"beta", "org.freedesktop.Platform/x86_64/beta", true, false},
		{"org.freedesktop.Platform/aarch64/26.08", "", false, true},
		{"-x", "", false, true},
		{"a/b", "", false, true},
	} {
		r, err := Resolve(orig, tc.target, branches)
		if (err != nil) != tc.err {
			t.Errorf("%q: err %v", tc.target, err)
			continue
		}
		if tc.err {
			continue
		}
		if r.Runtime.String() != tc.want || r.Forced != tc.forced || r.Original != orig {
			t.Errorf("%q: %+v, want %s forced %v", tc.target, r, tc.want, tc.forced)
		}
	}
	// latest with nothing newer known: upstream is already there
	if r, err := Resolve(orig, Latest, []string{"24.08"}); err != nil || r.Forced {
		t.Errorf("latest without newer: %+v %v", r, err)
	}
	if _, err := Resolve(pref("org.example.Platform/x86_64/stable"), Latest, []string{"26.08"}); err == nil {
		t.Errorf("latest of a branch of no known form")
	}
	kde := pref("org.kde.Platform/x86_64/5.15-24.08")
	if r, _ := Resolve(kde, Latest, []string{"6.9", "5.15-25.08"}); r.Runtime.Branch != "5.15-25.08" {
		t.Errorf("KDE 5.15 latest: %+v", r)
	}
	if r, _ := Resolve(kde, "6.9", nil); !r.Forced || r.Runtime.Branch != "6.9" {
		t.Errorf("KDE 5.15 to an explicit 6.9: %+v", r)
	}
}

func TestValidTarget(t *testing.T) {
	for _, ok := range []string{"", "latest", "26.08", "org.freedesktop.Platform/x86_64/26.08", "org.freedesktop.Platform//26.08"} {
		if err := ValidTarget(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"a/b", "x/y/", ".1", "26 08"} {
		if ValidTarget(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

const appMetadata = `# built by flatpak-builder
[Application]
name=com.example.App
runtime=org.freedesktop.Platform/x86_64/25.08
sdk=org.freedesktop.Sdk/x86_64/25.08
command=app
X-Unknown=kept

[Context]
shared=network;ipc;

[Extension org.freedesktop.Platform.ffmpeg-full]
directory=lib/ffmpeg
version=25.08
add-ld-path=.

[Extension org.freedesktop.Platform.GL]
versions=25.08;25.08-extra;1.4
version[de]=25.08

[Extension com.example.App.Plugin]
version=stable

[Extra Data]
name=app.deb
`

func TestRewriteMetadata(t *testing.T) {
	from := pref("org.freedesktop.Platform/x86_64/25.08")
	to := pref("org.freedesktop.Platform/x86_64/26.08")
	got, err := RewriteMetadata(appMetadata, from, to)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.NewReplacer(
		"runtime=org.freedesktop.Platform/x86_64/25.08", "runtime=org.freedesktop.Platform/x86_64/26.08",
		"sdk=org.freedesktop.Sdk/x86_64/25.08", "sdk=org.freedesktop.Sdk/x86_64/26.08",
		"version=25.08\nadd-ld-path", "version=26.08\nadd-ld-path",
		"versions=25.08;25.08-extra;1.4", "versions=26.08;25.08-extra;1.4",
	).Replace(appMetadata)
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	// Back again gives the original, byte for byte.
	back, err := RewriteMetadata(got, to, from)
	if err != nil || back != appMetadata {
		t.Errorf("back: %v\n%s", err, back)
	}
	// RuntimeOf and HasExtraData read the result.
	if p, err := RuntimeOf(got); err != nil || p != to {
		t.Errorf("RuntimeOf: %v %v", p, err)
	}
	if !HasExtraData(got) || HasExtraData("[Application]\nruntime=a/b/c\n") {
		t.Errorf("HasExtraData")
	}
}

func TestRewriteMetadataKeepsFormatting(t *testing.T) {
	in := "[Application]\r\nruntime = org.freedesktop.Platform/x86_64/25.08  \r\n  sdk=org.gnome.Sdk/x86_64/49\r\n[Extension X]\nversion =  25.08"
	got, err := RewriteMetadata(in, pref("org.freedesktop.Platform/x86_64/25.08"), pref("org.freedesktop.Platform/x86_64/26.08"))
	if err != nil {
		t.Fatal(err)
	}
	want := "[Application]\r\nruntime = org.freedesktop.Platform/x86_64/26.08  \r\n  sdk=org.gnome.Sdk/x86_64/49\r\n[Extension X]\nversion =  26.08"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestRewriteMetadataRefuses(t *testing.T) {
	from := pref("org.freedesktop.Platform/x86_64/25.08")
	to := pref("org.freedesktop.Platform/x86_64/26.08")
	for _, in := range []string{
		"[Runtime]\nruntime=org.freedesktop.Platform/x86_64/25.08\n",     // a runtime, not an app
		"[Application]\nruntime=org.freedesktop.Platform/x86_64/24.08\n", // not the runtime we think
		"",
	} {
		if _, err := RewriteMetadata(in, from, to); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestRecords(t *testing.T) {
	dir := t.TempDir()
	if HasRecords(dir) {
		t.Fatal("records in an empty dir")
	}
	if rs, err := Load(dir); err != nil || rs != nil {
		t.Fatalf("load empty: %v %v", rs, err)
	}
	a := Record{Ref: "app/b.App/x86_64/stable", Target: "latest", Remote: "flathub", Method: MethodPatch,
		Original: "org.freedesktop.Platform/x86_64/25.08", Runtime: "org.freedesktop.Platform/x86_64/26.08"}
	b := Record{Ref: "app/a.App/x86_64/stable", Target: "26.08", Remote: "flathub", Method: MethodReexport,
		Original: "org.freedesktop.Platform/x86_64/25.08", Runtime: "org.freedesktop.Platform/x86_64/26.08",
		Upstream: strings.Repeat("a", 64)}
	rs := Set(Set(nil, a), b)
	if err := Save(dir, rs); err != nil {
		t.Fatal(err)
	}
	if !HasRecords(dir) {
		t.Fatal("no records file")
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []Record{b, a}) { // sorted by ref
		t.Errorf("got %+v", got)
	}
	a.Target = "27.08"
	got = Set(got, a)
	if r, _ := Find(got, a.Ref); r.Target != "27.08" || len(got) != 2 {
		t.Errorf("Set did not replace: %+v", got)
	}
	if b.ID() != "a.App" {
		t.Errorf("ID %q", b.ID())
	}
	got = Remove(Remove(got, a.Ref), b.Ref)
	if err := Save(dir, got); err != nil {
		t.Fatal(err)
	}
	if HasRecords(dir) {
		t.Error("an empty list left the file")
	}
	// No temporary files left behind.
	left, _ := filepath.Glob(filepath.Join(Dir(dir), ".*"))
	if len(left) != 0 {
		t.Errorf("left: %v", left)
	}
	if err := os.WriteFile(RecordsPath(dir), []byte(`{"version":2,"apps":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Error("unknown version accepted")
	}
}

// fakeDeployment lays out an installed app the way flatpak does, with the
// metadata file a hard link to an "object" in the repository.
func fakeDeployment(t *testing.T, inst, ref, metadata string) Deployment {
	t.Helper()
	deploy, err := os.ReadFile("../gvariant/testdata/real.deploy")
	if err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("c", 64)
	dir := filepath.Join(inst, filepath.FromSlash(ref), commit)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	object := filepath.Join(inst, "repo", "objects", "ab", "cdef.file")
	os.MkdirAll(filepath.Dir(object), 0o755)
	if err := os.WriteFile(object, []byte(metadata), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(object, filepath.Join(dir, "metadata")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "deploy"), deploy, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(commit, filepath.Join(inst, filepath.FromSlash(ref), "active")); err != nil {
		t.Fatal(err)
	}
	d, ok, err := Active(inst, ref)
	if err != nil || !ok {
		t.Fatalf("Active: %v %v", ok, err)
	}
	return d
}

func TestPatch(t *testing.T) {
	inst := t.TempDir()
	ref := "app/org.rpak.Deploy/x86_64/stable"
	orig := "[Application]\nname=org.rpak.Deploy\nruntime=org.freedesktop.Platform/x86_64/25.08\nsdk=org.freedesktop.Sdk/x86_64/25.08\ncommand=run\n"
	d := fakeDeployment(t, inst, ref, orig)
	if d.Origin != "local" || len(d.Commit) != 64 {
		t.Errorf("deployment %+v", d)
	}
	if got := Installed(inst, "app"); !reflect.DeepEqual(got, []string{ref}) {
		t.Errorf("Installed %v", got)
	}
	text, err := RewriteMetadata(orig, pref("org.freedesktop.Platform/x86_64/25.08"), pref("org.freedesktop.Platform/x86_64/26.08"))
	if err != nil {
		t.Fatal(err)
	}
	changed, err := Patch(d, text)
	if err != nil || !changed {
		t.Fatalf("Patch: %v %v", changed, err)
	}
	if got, _ := d.MetadataFile(); got != text {
		t.Errorf("metadata %q", got)
	}
	// The repository object is untouched, the deployment's file is a new
	// one.
	object := filepath.Join(inst, "repo", "objects", "ab", "cdef.file")
	if b, _ := os.ReadFile(object); string(b) != orig {
		t.Errorf("the object was written through: %q", b)
	}
	var st syscall.Stat_t
	if err := syscall.Stat(object, &st); err != nil || st.Nlink != 1 {
		t.Errorf("object link count %d", st.Nlink)
	}
	// The deploy file has the new runtime, and only that changed: the
	// fixture's GLib counterpart.
	deploy, _ := os.ReadFile(filepath.Join(d.Dir, "deploy"))
	want, _ := os.ReadFile("../gvariant/testdata/real.forced.deploy")
	if string(deploy) != string(want) {
		t.Errorf("deploy file differs from GLib's")
	}
	// Again: nothing to do.
	if changed, err := Patch(d, text); err != nil || changed {
		t.Errorf("second Patch: %v %v", changed, err)
	}
	// And back.
	if changed, err := Patch(d, orig); err != nil || !changed {
		t.Errorf("Patch back: %v %v", changed, err)
	}
	deploy, _ = os.ReadFile(filepath.Join(d.Dir, "deploy"))
	if want, _ := os.ReadFile("../gvariant/testdata/real.deploy"); string(deploy) != string(want) {
		t.Errorf("deploy file not restored")
	}
	if left, _ := filepath.Glob(filepath.Join(d.Dir, ".*")); len(left) != 0 {
		t.Errorf("left: %v", left)
	}
}

func TestRuntimeInstalled(t *testing.T) {
	user, system := t.TempDir(), t.TempDir()
	p := pref("org.freedesktop.Platform/x86_64/26.08")
	if RuntimeInstalled([]string{user, system}, p) {
		t.Fatal("installed in empty dirs")
	}
	dir := filepath.Join(system, "runtime", p.ID, p.Arch, p.Branch)
	os.MkdirAll(filepath.Join(dir, "c"), 0o755)
	os.Symlink("c", filepath.Join(dir, "active"))
	if !RuntimeInstalled([]string{user, system}, p) {
		t.Error("not found in the system installation")
	}
	if got := InstalledBranches([]string{user, system}, p.ID, p.Arch); !reflect.DeepEqual(got, []string{"26.08"}) {
		t.Errorf("InstalledBranches %v", got)
	}
}

func TestDecideLocalCleanup(t *testing.T) {
	patch := Record{Ref: "app/a/x86_64/stable", Method: MethodPatch}
	reexport := Record{Ref: "app/b/x86_64/stable", Method: MethodReexport}
	for _, tc := range []struct {
		name    string
		records []Record
		users   []string
		exists  bool
		want    LocalCleanup
	}{
		{"the last reexport record went", []Record{patch}, nil, true, RemoveLocal},
		{"no records left at all", nil, nil, true, RemoveLocal},
		{"a reexport record is left", []Record{patch, reexport}, nil, true, KeepLocal},
		{"something still comes from rpak-local", []Record{patch}, []string{"app/b/x86_64/stable"}, true, KeepLocalInUse},
		{"nothing there", nil, nil, false, KeepLocal},
	} {
		if got := DecideLocalCleanup(tc.records, tc.users, tc.exists); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestBody(t *testing.T) {
	sum := strings.Repeat("e", 64)
	up, rt := ParseBody(Body(sum, pref("org.freedesktop.Platform/x86_64/26.08")))
	if up != sum || rt != "org.freedesktop.Platform/x86_64/26.08" {
		t.Errorf("%q %q", up, rt)
	}
	if up, rt := ParseBody("some upstream body"); up != "" || rt != "" {
		t.Errorf("%q %q", up, rt)
	}
}

// TestPatchesFileKeepsOtherKinds: the file is by app, then by kind of
// patch; kinds rpak does not know survive a rewrite.
func TestPatchesFileKeepsOtherKinds(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(Dir(dir), 0o755)
	in := `{"version":1,"apps":{
		"app/a.A/x86_64/stable":{"force-runtime":{"target":"latest","remote":"flathub","method":"patch","original_runtime":"x/y/1","runtime":"x/y/2"},"future":{"x":1}},
		"app/b.B/x86_64/stable":{"force-runtime":{"target":"2","remote":"flathub","method":"reexport","original_runtime":"x/y/1","runtime":"x/y/2","upstream_commit":"c"}}}}`
	os.WriteFile(RecordsPath(dir), []byte(in), 0o644)
	rs, err := Load(dir)
	if err != nil || len(rs) != 2 || rs[0].Ref != "app/a.A/x86_64/stable" || rs[1].Upstream != "c" {
		t.Fatalf("%+v %v", rs, err)
	}
	// Both force-runtime patches go: a.A keeps its other patch, b.B goes.
	if err := Save(dir, nil); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(RecordsPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(strings.Fields(string(b)), ""); got != `{"version":1,"apps":{"app/a.A/x86_64/stable":{"future":{"x":1}}}}` {
		t.Errorf("got %s", got)
	}
	if rs, _ := Load(dir); len(rs) != 0 {
		t.Errorf("records left: %+v", rs)
	}
}
