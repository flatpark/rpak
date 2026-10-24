package main

// Integration tests of rpak completion.

import (
	"bytes"
	"strings"
	"testing"
)

func TestCompletionScripts(t *testing.T) {
	e := newEnv(t)
	for _, shell := range []string{"bash", "zsh", "fish"} {
		cmd := e.command("completion", shell)
		out, err := cmd.Output()
		if err != nil || !strings.Contains(string(out), "command rpak complete ") {
			t.Errorf("%s: %v\n%s", shell, err, out)
		}
	}
	if out, err := e.command("-v", "completion", "zsh").Output(); err != nil || !strings.HasPrefix(string(out), "#compdef rpak\n") {
		t.Errorf("-v completion zsh: %v\n%s", err, out)
	}
	if out, err := e.command("completion", "--help").Output(); err != nil || !strings.Contains(string(out), "_rpak") {
		t.Errorf("--help: %v\n%s", err, out)
	}
	for _, argv := range [][]string{{"completion"}, {"completion", "nope"}, {"completion", "zsh", "bash"}} {
		cmd := e.command(argv...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		cmd.Run()
		if cmd.ProcessState.ExitCode() != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "Usage: rpak completion") {
			t.Errorf("%q: exit %d, stdout %q, stderr %q", argv, cmd.ProcessState.ExitCode(), stdout.String(), stderr.String())
		}
	}
	if e.read("calls") != "" {
		t.Error("flatpak was run")
	}
}
