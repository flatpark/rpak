package app

import (
	"testing"

	"github.com/flatpark/rpak/internal/args"
)

func TestAnswersNo(t *testing.T) {
	for line, want := range map[string]bool{
		"n\n": true, "N\n": true, "no\n": true, "No\n": true, "nO\n": true, "  no \t\n": true, "n": true,
		"\n": false, "y\n": false, "yes\n": false, "nope\n": false, "0\n": false, "n o\n": false,
		// g_strstrip() strips ASCII white space only.
		"n\u00a0\n": false,
	} {
		if got := answersNo([]byte(line)); got != want {
			t.Errorf("%q: %v, want %v", line, got, want)
		}
	}
}

func TestInteractiveNotWithAssumeYes(t *testing.T) {
	for _, argv := range [][]string{{"-y"}, {"-uy", "app"}, {"--assumeyes"}, {"--noninteractive"}} {
		if got := interactive(args.Parse("update", argv)); got != "-y" {
			t.Errorf("%q: %q", argv, got)
		}
	}
	// go test's stdout is not a terminal.
	if got := interactive(args.Parse("update", nil)); got != "not a terminal" {
		t.Errorf("no options: %q", got)
	}
}
