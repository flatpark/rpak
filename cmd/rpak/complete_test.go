package main

// Integration tests of rpak complete, with the fake flatpak answering
// "flatpak complete".

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// complete runs `rpak complete LINE POINT CUR` and returns its stdout and
// exit code; it fails the test if rpak printed anything on stderr.
func (e *env) complete(env []string, argv ...string) (string, int) {
	e.t.Helper()
	cmd := e.command(append([]string{"complete"}, argv...)...)
	cmd.Env = append(cmd.Env, env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		e.t.Fatal(err)
	}
	if stderr.Len() != 0 {
		e.t.Errorf("complete %q: stderr %q", argv, stderr.String())
	}
	return stdout.String(), code
}

// completeArgv is what the fake flatpak got as complete, without the
// "complete" itself.
func (e *env) completeArgv() []string {
	got := e.read("complete-argv")
	if got == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(got, "\x00"), "\x00")[1:]
}

func TestCompleteCommandNames(t *testing.T) {
	e := newEnv(t)
	e.write("complete-out", "install \nupdate \nlist \n")
	out, code := e.complete(nil, "rpak ", "5", "")
	if code != 0 || out != "install \nupdate \nlist \nclean \ncompletion \n" {
		t.Errorf("exit %d, output %q", code, out)
	}
	if got, want := e.completeArgv(), []string{"flatpak ", "8", ""}; !slices.Equal(got, want) {
		t.Errorf("flatpak got %q, want %q", got, want)
	}

	// The fork's flatpak has clean too: offered once.
	e.write("complete-out", "clean \n")
	if out, _ := e.complete(nil, "/usr/bin/rpak cl", "16", "cl"); out != "clean \n" {
		t.Errorf("output %q", out)
	}
	if got, want := e.completeArgv(), []string{"flatpak cl", "10", "cl"}; !slices.Equal(got, want) {
		t.Errorf("flatpak got %q, want %q", got, want)
	}

	// Stock flatpak has no clean.
	e.write("complete-out", "")
	for _, argv := range [][]string{{"rpak cl", "7", "cl"}, {"./rpak -v cl", "12", "cl"}} {
		if out, code := e.complete(nil, argv...); code != 0 || out != "clean \n" {
			t.Errorf("%q: exit %d, output %q", argv, code, out)
		}
	}
}

func TestCompleteCommandNamesWithoutFlatpak(t *testing.T) {
	e := newEnv(t)
	for _, flatpak := range []string{filepath.Join(e.fake, "missing"), "false"} {
		env := []string{"RPAK_FLATPAK=" + flatpak}
		if out, code := e.complete(env, "rpak ", "5", ""); code != 0 || out != "install \nupdate \nclean \ncompletion \n" {
			t.Errorf("%s: exit %d, output %q", flatpak, code, out)
		}
		if out, _ := e.complete(env, "rpak up", "7", "up"); out != "update \n" {
			t.Errorf("%s: output %q", flatpak, out)
		}
	}
}

func TestCompleteClean(t *testing.T) {
	e := newEnv(t)
	e.write("complete-out", "--delete-data \n")
	for _, c := range []struct {
		argv []string
		want string
	}{
		{[]string{"rpak clean --", "13", "--"}, "--verbose \n--ostree-verbose \n--help \n--dry-run \n--delete-data \n--user \n--system \n--installation=\n"},
		{[]string{"rpak clean -", "12", "-"}, "--verbose \n-v \n--ostree-verbose \n--help \n-h \n--dry-run \n--delete-data \n--user \n-u \n--system \n--installation=\n"},
		{[]string{"rpak -v clean --d", "17", "--d"}, "--dry-run \n--delete-data \n"},
		{[]string{"rpak -vv --ostree-verbose clean --d", "35", "--d"}, "--dry-run \n--delete-data \n"},
		{[]string{"rpak clean --de", "15", "--de"}, "--delete-data \n"},
		{[]string{"rpak clean --delete-data --d", "28", "--d"}, "--dry-run \n"},
		// Switches already given are not offered again; -v may be repeated.
		{[]string{"rpak clean --dry-run -uv --ostree-verbose -", "43", "-"}, "--verbose \n-v \n--help \n-h \n--delete-data \n--system \n--installation=\n"},
		{[]string{"rpak clean --user --us", "22", "--us"}, ""},
		// clean takes no arguments.
		{[]string{"rpak clean ", "11", ""}, ""},
		{[]string{"rpak clean x", "12", "x"}, ""},
	} {
		os.Remove(filepath.Join(e.fake, "complete-argv"))
		out, code := e.complete(nil, c.argv...)
		if code != 0 || out != c.want {
			t.Errorf("%q: exit %d, output %q, want %q", c.argv, code, out, c.want)
		}
		if e.read("complete-argv") != "" {
			t.Errorf("%q: flatpak was asked", c.argv)
		}
	}

	// Not rpak's clean: rpak would pass "--user clean" to flatpak.
	if out, _ := e.complete(nil, "rpak --user clean --d", "21", "--d"); out != "--delete-data \n" {
		t.Errorf("output %q", out)
	}
	if got, want := e.completeArgv(), []string{"flatpak --user clean --d", "24", "--d"}; !slices.Equal(got, want) {
		t.Errorf("flatpak got %q, want %q", got, want)
	}
}

func TestCompleteCleanInstallations(t *testing.T) {
	e := newEnv(t)
	e.write("complete-out", "nothing \n")
	if err := os.MkdirAll(filepath.Join(e.conf, "installations.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(e.conf, "installations.d", "extra.conf"), []byte(
		"[Installation \"extra\"]\nPath=/nonexistent/extra\nPriority=10\n\n"+
			"[Installation \"low\"]\nPath=/nonexistent/low\nPriority=-5\n"), 0o644)
	for _, c := range []struct {
		argv []string
		want string
	}{
		// zsh passes the whole word, bash the part after "=": the output is
		// the same, only the value.
		{[]string{"rpak clean --installation=", "26", "--installation="}, "extra \ndefault \nlow \n"},
		{[]string{"rpak clean --installation=", "26", ""}, "extra \ndefault \nlow \n"},
		{[]string{"rpak clean --installation=de", "28", "--installation=de"}, "default \n"},
		{[]string{"rpak clean --installation=de", "28", "de"}, "default \n"},
		{[]string{"rpak clean --installation ", "26", ""}, "extra \ndefault \nlow \n"},
		{[]string{"rpak clean --installation l", "27", "l"}, "low \n"},
		{[]string{"rpak clean --inst", "17", "--inst"}, "--installation=\n"},
	} {
		if out, code := e.complete(nil, c.argv...); code != 0 || out != c.want {
			t.Errorf("%q: exit %d, output %q, want %q", c.argv, code, out, c.want)
		}
	}
}

func TestCompleteOtherCommandsAskFlatpak(t *testing.T) {
	e := newEnv(t)
	verbatim := "--user \n__FLATPAK_BUNDLE_OR_REF_FILE\nodd output\twith a tab\n"
	e.write("complete-out", verbatim)
	for _, c := range []struct {
		argv []string
		want []string
	}{
		{[]string{"rpak install --us", "17", "--us"}, []string{"flatpak install --us", "20", "--us"}},
		{[]string{"rpak update ", "12", ""}, []string{"flatpak update ", "15", ""}},
		{[]string{"rpak upgrade org.", "17", "org."}, []string{"flatpak upgrade org.", "20", "org."}},
		{[]string{"rpak info org.fr", "16", "org.fr"}, []string{"flatpak info org.fr", "19", "org.fr"}},
		{[]string{"  'rpak' -v remote-ls fl", "24", "fl"}, []string{"  flatpak -v remote-ls fl", "25", "fl"}},
		// The cursor in the middle of the line: flatpak gets all of it.
		{[]string{"rpak install  --user", "13", ""}, []string{"flatpak install  --user", "16", ""}},
		// POINT counts characters, flatpak counts bytes.
		{[]string{"rpak install ü --us", "19", "--us"}, []string{"flatpak install ü --us", "23", "--us"}},
		{[]string{"rpak install --us", "999", "--us"}, []string{"flatpak install --us", "20", "--us"}},
		{[]string{"rpak install --us", "17"}, []string{"flatpak install --us", "20", ""}},
	} {
		os.Remove(filepath.Join(e.fake, "complete-argv"))
		out, code := e.complete(nil, c.argv...)
		if code != 0 || out != verbatim {
			t.Errorf("%q: exit %d, output %q", c.argv, code, out)
		}
		if got := e.completeArgv(); !slices.Equal(got, c.want) {
			t.Errorf("%q: flatpak got %q, want %q", c.argv, got, c.want)
		}
	}

	e.write("complete-exit", "3")
	if out, code := e.complete(nil, "rpak list --", "12", "--"); code != 3 || out != verbatim {
		t.Errorf("exit %d, want flatpak's 3; output %q", code, out)
	}
}

func TestCompleteOddInput(t *testing.T) {
	e := newEnv(t)
	e.write("complete-out", "list \n")
	for _, argv := range [][]string{
		{"", "0", ""},
		{"", "5", ""},
		{"rpak", "4", "rpak"},
		{"rpak ", "-3", ""},
		{"rpak ", "x", ""},
		{"rpak ", "", ""},
		{"   ", "3", ""},
		{"rpak 'clean --", "14", "--"},
		{"rpak \"clean --", "14", "--"},
		{"ü", "1", "ü"},
	} {
		out, code := e.complete(nil, argv...)
		if code != 1 || out != "" {
			t.Errorf("%q: exit %d, output %q", argv, code, out)
		}
	}
	for _, c := range []struct {
		argv []string
		want string
	}{
		{[]string{"rpak cl", "100", "cl"}, "list \nclean \n"},
		{[]string{"rpak ü cl", "9", "cl"}, "list \n"},
		{[]string{"rpak \xff\xfe clean -", "100", "-"}, "list \n"},
		{[]string{"rpak clean \"ü x\" 'y' --sys", "26", "--sys"}, "--system \n"},
		{[]string{"rpak clean ü --sys", "18", "--sys"}, "--system \n"},
		{[]string{"rpak \tclean\n--d", "15", "--d"}, "--dry-run \n--delete-data \n"},
	} {
		if out, code := e.complete(nil, c.argv...); code != 0 || out != c.want {
			t.Errorf("%q: exit %d, output %q, want %q", c.argv, code, out, c.want)
		}
	}

	// "complete" without LINE and POINT is not the protocol: flatpak's.
	if out := e.run("complete", "x"); out.code != 0 || e.read("complete-argv") != "complete\x00x\x00" {
		t.Errorf("complete x: exit %d, flatpak got %q", out.code, e.read("complete-argv"))
	}
}

func TestCompleteChangesNothing(t *testing.T) {
	e := newEnv(t)
	e.home = t.TempDir()
	e.write("complete-out", "--user \n")
	env := []string{"RPAK_DRY_RUN=1"}
	for _, argv := range [][]string{
		{"rpak ", "5", ""},
		{"rpak install --us", "17", "--us"},
		{"rpak update --us", "16", "--us"},
		{"rpak clean -", "12", "-"},
	} {
		if out, _ := e.complete(env, argv...); out == "" {
			t.Errorf("%q: no output with RPAK_DRY_RUN=1", argv)
		}
	}
	for _, dir := range []string{e.base, e.sys, e.home} {
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("%s: complete created %v", dir, entries)
		}
	}
	// Only "complete": no planning queries.
	for _, call := range strings.Split(strings.TrimSpace(e.read("calls")), "\n") {
		if !strings.HasPrefix(call, "complete ") {
			t.Errorf("flatpak called with %q", call)
		}
	}
}
