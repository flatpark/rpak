package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flatpark/rpak/internal/extradata"
	fr "github.com/flatpark/rpak/internal/forceruntime"
	"github.com/flatpark/rpak/internal/installation"
	"github.com/flatpark/rpak/internal/metadata"
)

// forcedInstallation lays out a user installation with org.rpak.Deploy
// deployed, its commit (with xa.metadata) in the repository, and the
// deployment's metadata either upstream's or forced to 26.08.
func forcedInstallation(t *testing.T, rewritten bool) (inst, commit string) {
	t.Helper()
	inst = t.TempDir()
	sum, err := os.ReadFile("../forceruntime/testdata/app.commit.sum")
	if err != nil {
		t.Fatal(err)
	}
	commit = strings.TrimSpace(string(sum))
	obj, _ := os.ReadFile("../forceruntime/testdata/app.commit")
	objPath := filepath.Join(inst, "repo", "objects", commit[:2], commit[2:]+".commit")
	os.MkdirAll(filepath.Dir(objPath), 0o755)
	os.WriteFile(objPath, obj, 0o644)
	orig, err := fr.CommitMetadata(inst, commit)
	if err != nil {
		t.Fatal(err)
	}
	text := orig
	if rewritten {
		from, _ := fr.ParsePref("org.freedesktop.Platform/x86_64/25.08")
		to, _ := fr.ParsePref("org.freedesktop.Platform/x86_64/26.08")
		if text, err = fr.RewriteMetadata(orig, from, to); err != nil {
			t.Fatal(err)
		}
	}
	refDir := filepath.Join(inst, "app", "org.rpak.Deploy", "x86_64", "stable")
	dir := filepath.Join(refDir, commit)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "metadata"), []byte(text), 0o644)
	deploy, _ := os.ReadFile("../gvariant/testdata/real.deploy")
	os.WriteFile(filepath.Join(dir, "deploy"), deploy, 0o644)
	os.Symlink(commit, filepath.Join(refDir, "active"))
	return inst, commit
}

const deployRef = "app/org.rpak.Deploy/x86_64/stable"

// appItems is the app section of org.rpak.Deploy in the user
// installation inst, with rpak's records as forced() finds them.
func appItems(t *testing.T, inst, commit string) []string {
	t.Helper()
	env := func(k string) (string, bool) {
		if k == "FLATPAK_USER_DIR" {
			return inst, true
		}
		return "", false
	}
	d := &doctor{cfg: Config{LookupEnv: env, Home: t.TempDir()}}
	d.force = d.forced()
	in := &instInfo{exists: true, inst: d.mustUser(t)}
	info := map[string]string{"Ref": deployRef, "Commit": commit, "Origin": "flathub",
		"Runtime": "org.freedesktop.Platform/x86_64/26.08"}
	a := &appInfo{id: "org.rpak.Deploy", checked: true, insts: []*appInst{{in: in, info: info}}}
	var out []string
	for _, it := range d.appSection(a, []*instInfo{in}, psInfo{}).Items {
		out = append(out, it.Text)
	}
	return out
}

func (d *doctor) mustUser(t *testing.T) installation.Installation {
	u, err := d.installEnv().User()
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func grepItems(items []string, s string) []string {
	var out []string
	for _, it := range items {
		if strings.Contains(it, s) {
			out = append(out, it)
		}
	}
	return out
}

func TestForcedAppIsNotAProblem(t *testing.T) {
	inst, commit := forcedInstallation(t, true)
	rec := fr.Record{Ref: deployRef, Target: "latest", Remote: "flathub", Method: fr.MethodPatch,
		Original: "org.freedesktop.Platform/x86_64/25.08", Runtime: "org.freedesktop.Platform/x86_64/26.08"}
	if err := fr.Save(inst, []fr.Record{rec}); err != nil {
		t.Fatal(err)
	}
	items := appItems(t, inst, commit)
	if got := grepItems(items, "forced runtime: built for org.freedesktop.Platform/x86_64/25.08, forced to org.freedesktop.Platform/x86_64/26.08  (rpak patch force-runtime, method patch, upstream remote flathub)"); len(got) != 1 {
		t.Errorf("no forced line:\n%s", strings.Join(items, "\n"))
	}
	if got := grepItems(items, "rewritten by rpak patch force-runtime"); len(got) != 1 {
		t.Errorf("the rewrite is not attributed to rpak:\n%s", strings.Join(items, "\n"))
	}
	if got := grepItems(items, "differs from its commit"); len(got) != 0 {
		t.Errorf("reported as an unexplained difference: %q", got)
	}
}

func TestRewrittenWithoutRecordIsReported(t *testing.T) {
	inst, commit := forcedInstallation(t, true)
	items := appItems(t, inst, commit)
	want := "deployment's metadata: differs from its commit's xa.metadata (runtime org.freedesktop.Platform/x86_64/26.08, the commit's org.freedesktop.Platform/x86_64/25.08)"
	if got := grepItems(items, "deployment's metadata"); len(got) != 1 || got[0] != want {
		t.Errorf("got %q", got)
	}
	if got := grepItems(items, "forced runtime"); len(got) != 0 {
		t.Errorf("forced without a record: %q", got)
	}
}

func TestUnchangedDeploymentSaysNothing(t *testing.T) {
	inst, commit := forcedInstallation(t, false)
	items := appItems(t, inst, commit)
	if got := grepItems(items, "metadata"); len(got) != 0 {
		t.Errorf("got %q", got)
	}
}

func TestForcedSection(t *testing.T) {
	// The deployment is upstream's again: flatpak update outside rpak.
	inst, _ := forcedInstallation(t, false)
	records := []fr.Record{
		{Ref: deployRef, Target: "26.08", Remote: "flathub", Method: fr.MethodPatch,
			Original: "org.freedesktop.Platform/x86_64/25.08", Runtime: "org.freedesktop.Platform/x86_64/26.08"},
		{Ref: "app/org.rpak.Gone/x86_64/stable", Remote: "flathub", Method: fr.MethodReexport,
			Original: "org.freedesktop.Platform/x86_64/25.08", Runtime: "org.freedesktop.Platform/x86_64/26.08"},
	}
	if err := fr.Save(inst, records); err != nil {
		t.Fatal(err)
	}
	env := func(k string) (string, bool) {
		if k == "FLATPAK_USER_DIR" {
			return inst, true
		}
		return "", false
	}
	d := &doctor{cfg: Config{LookupEnv: env, Home: t.TempDir()}}
	f := d.forced()
	s, ok := d.forcedSection(f, true)
	if !ok {
		t.Fatal("no section")
	}
	var items []string
	for _, it := range s.Items {
		items = append(items, it.Text)
	}
	text := strings.Join(items, "\n")
	for _, want := range []string{
		"org.rpak.Deploy/x86_64/stable  force-runtime (method patch): built for org.freedesktop.Platform/x86_64/25.08, forced to org.freedesktop.Platform/x86_64/26.08  (target 26.08, upstream remote flathub); deployment runs on org.freedesktop.Platform/x86_64/25.08: not forced now",
		"org.rpak.Gone/x86_64/stable  force-runtime (method reexport): built for org.freedesktop.Platform/x86_64/25.08, forced to org.freedesktop.Platform/x86_64/26.08  (target latest, upstream remote flathub); not installed",
		"remote rpak-local: " + fr.RepoPath(inst),
		"local repository " + fr.RepoPath(inst) + ": does not exist",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("lacks %q:\n%s", want, text)
		}
	}
	// Nothing forced, no remote: no section.
	d2 := &doctor{cfg: Config{LookupEnv: func(k string) (string, bool) {
		if k == "FLATPAK_USER_DIR" {
			return t.TempDir(), true
		}
		return "", false
	}, Home: t.TempDir()}}
	if _, ok := d2.forcedSection(d2.forced(), false); ok {
		t.Error("a section without anything forced")
	}
	// A broken records file is reported, not fatal.
	os.WriteFile(fr.RecordsPath(inst), []byte("{"), 0o644)
	s, ok = d.forcedSection(d.forced(), false)
	if !ok || !strings.Contains(s.Items[0].Text, "patches.json") {
		t.Errorf("broken records: %+v", s)
	}
}

// TestExtraDataPatch: a record of replaced extra-data shows in the
// Patches section, with the URL used and upstream's, and in the app's
// section; an app with that record coming from rpak-local is no orphan.
func TestExtraDataPatch(t *testing.T) {
	inst, commit := forcedInstallation(t, false)
	sum := strings.Repeat("e", 64)
	rec := fr.ExtraRecord{Ref: deployRef, Remote: "flathub", Upstream: strings.Repeat("c", 64),
		Sources: []extradata.Replacement{{Name: "app.deb", File: extradata.File{URL: "https://new.example.com/app_2.deb", SHA256: sum, Size: 2048},
			Base: extradata.File{URL: "https://old.example.com/app_1.deb", SHA256: strings.Repeat("a", 64), Size: 1024}}}}
	if err := fr.SaveExtra(inst, []fr.ExtraRecord{rec}); err != nil {
		t.Fatal(err)
	}
	items := appItems(t, inst, commit)
	if got := grepItems(items, "extra-data replaced: app.deb from https://new.example.com/app_2.deb instead of upstream's https://old.example.com/app_1.deb  (rpak patch extra-data, upstream remote flathub)"); len(got) != 1 {
		t.Errorf("no extra-data line:\n%s", strings.Join(items, "\n"))
	}
	if got := grepItems(items, "differs from its commit"); len(got) != 0 {
		t.Errorf("reported as an unexplained difference: %q", got)
	}
	env := func(k string) (string, bool) {
		if k == "FLATPAK_USER_DIR" {
			return inst, true
		}
		return "", false
	}
	d := &doctor{cfg: Config{LookupEnv: env, Home: t.TempDir()}}
	f := d.forced()
	// As if it came from rpak-local: no orphan.
	f.fromRepo = []string{deployRef}
	s, ok := d.forcedSection(f, true)
	if !ok {
		t.Fatal("no section")
	}
	var lines []string
	for _, it := range s.Items {
		lines = append(lines, it.Text)
	}
	text := strings.Join(lines, "\n")
	// The fixture's deployment comes from a remote of its own, so the
	// replacement is not in effect.
	want := "org.rpak.Deploy/x86_64/stable  extra-data: app.deb from https://new.example.com/app_2.deb (2.0 kB, sha256 " + sum +
		") instead of upstream's https://old.example.com/app_1.deb  (upstream remote flathub, upstream commit cccccccccccc); installed from "
	if !strings.Contains(text, want) || !strings.Contains(text, ", not rpak-local: not replaced now") {
		t.Errorf("lacks %q:\n%s", want, text)
	}
	if strings.Contains(text, "has no record") || strings.Contains(text, "no patches") {
		t.Errorf("an extra-data record taken for none:\n%s", text)
	}
	if got := deployedSource([]metadata.Source{{Name: "app.deb", URI: "https://new.example.com/app_2.deb", Checksum: sum}}, rec.Sources[0]); got != "has it" {
		t.Errorf("deployedSource: %s", got)
	}
	if got := deployedSource([]metadata.Source{{Name: "app.deb", URI: "https://old.example.com/app_1.deb", Checksum: "x"}}, rec.Sources[0]); got != "has https://old.example.com/app_1.deb (sha256 x)" {
		t.Errorf("deployedSource: %s", got)
	}
}
