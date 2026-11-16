package main

// Integration tests of rpak doctor's command line; what it collects is
// tested in internal/doctor, without the network.

import (
	"bytes"
	"strings"
	"testing"
)

func TestDoctorUsage(t *testing.T) {
	e := newEnv(t)
	for _, argv := range [][]string{{"doctor", "--help"}, {"doctor", "-h"}, {"-v", "doctor", "org.a.App", "--help"}} {
		out, err := e.command(argv...).Output()
		if err != nil || !strings.HasPrefix(string(out), "Usage: rpak doctor [APP_ID]...") {
			t.Errorf("%q: %v\n%s", argv, err, out)
		}
	}
	for _, argv := range [][]string{{"doctor", "--json"}, {"doctor", "-x", "org.a.App"}} {
		cmd := e.command(argv...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		cmd.Run()
		if cmd.ProcessState.ExitCode() != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "rpak doctor: unknown option") {
			t.Errorf("%q: exit %d, stdout %q, stderr %q", argv, cmd.ProcessState.ExitCode(), stdout.String(), stderr.String())
		}
	}
	if e.read("calls") != "" {
		t.Error("flatpak was run")
	}
}
