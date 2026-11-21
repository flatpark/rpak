package app

import (
	"reflect"
	"testing"

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
	if got := experimental("com.example.App", from, to); got != want {
		t.Errorf("got %q", got)
	}
}
