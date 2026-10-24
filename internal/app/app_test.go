package app

import (
	"os"
	"testing"
)

func TestShellQuote(t *testing.T) {
	got := shellQuote([]string{"/usr/bin/flatpak", "install", "--subpath=/a b", "it's", "", "org.example.App//stable"})
	want := `/usr/bin/flatpak install '--subpath=/a b' 'it'\''s' '' org.example.App//stable`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestJobs(t *testing.T) {
	warned := 0
	warn := func(string, ...any) { warned++ }
	for v, want := range map[string]int{"": 4, "1": 1, "16": 16, "0": 4, "17": 4, "x": 4} {
		t.Setenv("RPAK_JOBS", v)
		if got := jobs(warn); got != want {
			t.Errorf("RPAK_JOBS=%q: got %d, want %d", v, got, want)
		}
	}
	if warned != 3 {
		t.Errorf("warned %d times, want 3", warned)
	}
}

func TestIsTerminal(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	// A character device, but not a terminal.
	if isTerminal(devNull) {
		t.Error("/dev/null is reported as a terminal")
	}
}
