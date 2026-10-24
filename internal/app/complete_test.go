package app

import (
	"slices"
	"strings"
	"testing"
)

func TestShellSplit(t *testing.T) {
	for line, want := range map[string][]string{
		"rpak install ":              {"rpak", "install"},
		"  a\tb\nc ":                 {"a", "b", "c"},
		`'a b'"c d"e `:               {"a bc de"},
		`"a\"b\\c\$d\e" `:            {`a"b\c$d\e`},
		`a\ b\'c `:                   {"a b'c"},
		"a\\\nb ":                    {"ab"},
		"'' x ":                      {"", "x"},
		"a #comment 'unclosed\nb # ": {"a", "b"},
		"ü \xff ":                    {"ü", "\xff"},
	} {
		words, err := shellSplit(line)
		var got []string
		for _, w := range words {
			got = append(got, w.text)
		}
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("%q: got %q, %v; want %q", line, got, err, want)
		}
	}
	for _, line := range []string{"", "  \t", "# only a comment", "a 'b", `a "b`, `a "b\"`, `a \`} {
		if words, err := shellSplit(line); err == nil {
			t.Errorf("%q: got %v, want an error", line, words)
		}
	}
	words, _ := shellSplit(` 'rp ak' x`)
	if w := words[0]; w.start != 1 || w.end != 8 {
		t.Errorf("first word at %d-%d, want 1-8", w.start, w.end)
	}
}

func TestPickWordAt(t *testing.T) {
	for _, c := range []struct {
		line  string
		point int
		word  string
		begin int
	}{
		{"", 0, "", -1},
		{"rpak ", 5, "", 5},
		{"rpak cl", 7, "cl", 5},
		{"rpak cl", 6, "cl", 5},          // the word goes on past the cursor
		{"rpak clean --", 7, "clean", 5}, // as in flatpak
		{"rpak  x", 5, "", 5},
		{"rpak  x", 4, "rpak", 0},
		{" x", 0, "", 0},
	} {
		word, begin := pickWordAt(c.line, c.point)
		if word != c.word || begin != c.begin {
			t.Errorf("%q at %d: got %q at %d, want %q at %d", c.line, c.point, word, begin, c.word, c.begin)
		}
	}
}

func TestRuneOffset(t *testing.T) {
	for _, c := range []struct {
		s    string
		n    int
		want int
	}{
		{"abc", 2, 2}, {"abc", 9, 3}, {"abc", -1, 0}, {"üb", 1, 2}, {"üb", 2, 3}, {"\xff\xfeb", 2, 2},
	} {
		if got := runeOffset(c.s, c.n); got != c.want {
			t.Errorf("runeOffset(%q, %d) = %d, want %d", c.s, c.n, got, c.want)
		}
	}
}

func TestCompleteWord(t *testing.T) {
	for _, c := range []struct {
		cur, shellCur, candidate string
		want                     string
		ok                       bool
	}{
		{"cl", "cl", "clean ", "clean ", true},
		{"cl", "cl", "install ", "", false},
		{"", "", "clean ", "clean ", true},
		// zsh passes the whole word, bash what follows the "=".
		{"--installation=", "--installation=", "--installation=default ", "default ", true},
		{"--installation=de", "--installation=de", "--installation=default ", "default ", true},
		{"--installation=de", "de", "--installation=default ", "default ", true},
		{"--installation=", "", "--installation=default ", "default ", true},
		// bash keeps "a:b" together with _get_cword :, flatpak does not care.
		{"a:b", "a:b", "a:bc ", "a:bc ", true},
	} {
		comp := completion{cur: c.cur, shellCur: c.shellCur}
		got, ok := comp.completeWord(c.candidate)
		if got != c.want || ok != c.ok {
			t.Errorf("cur %q, shell %q, %q: got %q %v, want %q %v", c.cur, c.shellCur, c.candidate, got, ok, c.want, c.ok)
		}
	}
}

// Every option offered for clean is one parseCleanArgs takes and
// cleanUsage describes.
func TestCleanFlagsMatchParser(t *testing.T) {
	for _, f := range cleanFlags {
		args := []string{"--" + f.long}
		if f.value {
			args = []string{"--" + f.long + "=x"}
		}
		if f.short != 0 {
			args = append(args, "-"+string(f.short))
		}
		for _, arg := range args {
			if _, _, usageErr := parseCleanArgs([]string{arg}); usageErr != "" {
				t.Errorf("%s: %s", arg, usageErr)
			}
		}
		if f.long != "ostree-verbose" && !strings.Contains(cleanUsage, "--"+f.long) {
			t.Errorf("--%s is not in cleanUsage", f.long)
		}
	}
}
