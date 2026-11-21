package app

import (
	"reflect"
	"strings"
	"testing"

	"github.com/flatpark/rpak/internal/extradata"
	fr "github.com/flatpark/rpak/internal/forceruntime"
)

func TestResetCommands(t *testing.T) {
	records := []fr.Record{
		{Ref: "app/a.A/x86_64/stable", Remote: "flathub", Method: fr.MethodPatch},
		{Ref: "app/b.B/x86_64/stable", Remote: "flatpark", Method: fr.MethodReexport},
		{Ref: "app/c.C/x86_64/beta", Remote: "flathub", Method: fr.MethodReexport},
	}
	got := resetCommands(records, []string{"-y"})
	want := [][]string{
		{"install", "--user", "--reinstall", "-y", "flathub", "app/a.A/x86_64/stable", "app/c.C/x86_64/beta"},
		{"install", "--user", "--reinstall", "-y", "flatpark", "app/b.B/x86_64/stable"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d commands", len(got))
	}
	for i := range want {
		if !reflect.DeepEqual(got[i].argv, want[i]) {
			t.Errorf("command %d: %q, want %q", i, got[i].argv, want[i])
		}
	}
	if len(got[0].records) != 2 || got[1].records[0].Ref != "app/b.B/x86_64/stable" {
		t.Errorf("records: %+v", got)
	}
	if got := resetCommands(nil, nil); len(got) != 0 {
		t.Errorf("nothing to reset: %+v", got)
	}
}

func TestInstallOptions(t *testing.T) {
	// update's --commit (with its value) and --force-remove are not
	// install's; the rest is.
	got := installOptions([]string{"-y", "--commit", "abc", "--arch", "x86_64", "--force-remove", "--no-related", "--user", "--subpath=/x"})
	want := []string{"-y", "--arch", "x86_64", "--no-related", "--user", "--subpath=/x"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExperimentalLine(t *testing.T) {
	from, _ := fr.ParsePref("org.freedesktop.Platform/x86_64/25.08")
	to, _ := fr.ParsePref("org.freedesktop.Platform/x86_64/26.08")
	want := "rpak: experimental: com.example.App was built for org.freedesktop.Platform/x86_64/25.08, now runs on org.freedesktop.Platform/x86_64/26.08; it may break. Undo: rpak patch --reset com.example.App"
	if got := experimental("com.example.App", from, to, "--reset"); got != want {
		t.Errorf("got %q", got)
	}
	// With extra-data replaced too, plain --reset would undo that as well.
	want = strings.Replace(want, "--reset com", "--reset=force-runtime com", 1)
	if got := experimental("com.example.App", from, to, "--reset=force-runtime"); got != want {
		t.Errorf("got %q", got)
	}
}

func TestExperimentalExtraLines(t *testing.T) {
	sum := strings.Repeat("e", 64)
	rep := extradata.Replacement{Name: "app.deb", File: extradata.File{URL: "https://new/app_2.deb", SHA256: sum, Size: 2048},
		Base: extradata.File{URL: "https://old/app_1.deb"}}
	r := fr.ExtraRecord{Ref: "app/com.example.App/x86_64/stable", Sources: []extradata.Replacement{rep}}
	want := []string{
		"rpak: experimental: com.example.App downloads its extra-data app.deb from https://new/app_2.deb (2.0 kB, sha256 " + sum + ") instead of https://old/app_1.deb; that file is your choice, its packagers did not pick it.",
		"rpak: Undo: rpak patch --reset=extra-data com.example.App",
	}
	if got := experimentalExtra(r, fr.ExtraRecord{}); !reflect.DeepEqual(got, want) {
		t.Errorf("new: %q", got)
	}
	// Unchanged: nothing; a source gone, none new: nothing either.
	if got := experimentalExtra(r, r); got != nil {
		t.Errorf("unchanged: %q", got)
	}
	two := r
	two.Sources = append([]extradata.Replacement{{Name: "a.deb", File: extradata.File{URL: "https://x/a.deb"}}}, rep)
	if got := experimentalExtra(r, two); got != nil {
		t.Errorf("one gone: %q", got)
	}
	// Only the new one is told.
	if got := experimentalExtra(two, r); len(got) != 2 || !strings.Contains(got[0], " a.deb from https://x/a.deb ") {
		t.Errorf("one more: %q", got)
	}
}

// TestRefreshAction: what rpak update makes of a reexported app when
// upstream publishes a commit, with the extra-data replacements carried
// over to it (extradata.Carry: kept while upstream's file is the base),
// alone or with a forced runtime.
func TestRefreshAction(t *testing.T) {
	base := strings.Repeat("a", 64)
	rep := extradata.Replacement{Name: "app.deb", File: extradata.File{URL: "https://new/app_2.deb", SHA256: strings.Repeat("e", 64), Size: 2},
		Base: extradata.File{URL: "https://old/app_1.deb", SHA256: base, Size: 1}}
	meta := func(sum string) string {
		return "[Application]\nruntime=org.example.Platform/x86_64/1\n[Extra Data]\nname=app.deb\nchecksum=" + sum + "\nsize=1\nuri=https://old/app_1.deb\n"
	}
	orig, _ := fr.ParsePref("org.example.Platform/x86_64/1")
	forced, _ := fr.ParsePref("org.example.Platform/x86_64/2")
	old, now := strings.Repeat("1", 64), strings.Repeat("2", 64)
	for _, c := range []struct {
		desc         string
		upstreamSum  string // upstream's checksum of app.deb in its latest commit
		commit       string // upstream's latest commit
		runtimePatch bool
		want         int
		wantExtra    int // replacements left
	}{
		{"same commit, nothing to do", base, old, false, keepExport, 1},
		{"new commit, same file: the patch goes on", base, now, false, reexport, 1},
		{"new commit, upstream changed the file: back upstream", strings.Repeat("c", 64), now, false, goBack, 0},
		{"new commit, same file, runtime forced too", base, now, true, reexport, 1},
		{"upstream changed the file, runtime forced: reexported with the runtime alone", strings.Repeat("c", 64), now, true, reexport, 0},
		{"same commit, runtime forced", base, old, true, keepExport, 1},
	} {
		kept, _ := extradata.Carry([]extradata.Replacement{rep}, meta(c.upstreamSum), "app/a/x86_64/stable")
		it := item{remoteRef: remoteRef{ref: "app/a/x86_64/stable", commit: c.commit}, runtimePatch: c.runtimePatch, extra: kept,
			res: fr.Resolution{Original: orig, Runtime: orig}}
		exported := fr.Job{Ref: it.ref, Upstream: old, Runtime: it.res, ExtraData: []extradata.Replacement{rep}}
		if c.runtimePatch {
			it.res = fr.Resolution{Original: orig, Runtime: forced, Forced: true}
			exported.Runtime = it.res
		}
		if got := refreshAction(it, exported.Mark(), true, true); got != c.want || len(kept) != c.wantExtra {
			t.Errorf("%s: %d with %d replacements, want %d with %d", c.desc, got, len(kept), c.want, c.wantExtra)
		}
	}
	// Nothing exported, or the remote deleted by hand: export again.
	it := item{remoteRef: remoteRef{ref: "app/a/x86_64/stable", commit: old}, extra: []extradata.Replacement{rep}, res: fr.Resolution{Original: orig, Runtime: orig}}
	m := it.job().Mark()
	if refreshAction(it, m, false, true) != reexport || refreshAction(it, m, true, false) != reexport || refreshAction(it, m, true, true) != keepExport {
		t.Errorf("export or remote missing")
	}
}

func TestWriteRecords(t *testing.T) {
	var b strings.Builder
	writeRecords(&b, []fr.Record{
		{Ref: "app/org.a.App/x86_64/stable", Original: "org.freedesktop.Platform/x86_64/25.08", Runtime: "org.freedesktop.Platform/x86_64/26.08"},
		{Ref: "app/org.a.App/x86_64/beta", Target: "26.08", Original: "org.freedesktop.Platform/x86_64/25.08", Runtime: "org.freedesktop.Platform/x86_64/26.08"},
		{Ref: "app/org.b.App/x86_64/stable", Target: "org.gnome.Platform/x86_64/49", Original: "org.freedesktop.Platform/x86_64/25.08", Runtime: "org.gnome.Platform/x86_64/49"},
	})
	want := "App                      Runtime\n" +
		"org.a.App/x86_64/stable  org.freedesktop.Platform 25.08 → 26.08\n" +
		"org.a.App/x86_64/beta    org.freedesktop.Platform 25.08 → 26.08 (pinned)\n" +
		"org.b.App                org.freedesktop.Platform/x86_64/25.08 → org.gnome.Platform/x86_64/49 (pinned)\n"
	if b.String() != want {
		t.Errorf("got\n%s\nwant\n%s", b.String(), want)
	}
}
