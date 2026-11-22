package main

// Integration tests of rpak self-update's command line. Nothing here may
// get as far as replacing a binary: the binary would be the test binary.
// Replacing is tested in internal/app, on a fake binary.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSelfUpdateCommand(t *testing.T) {
	e := newEnv(t)
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/releases/latest" {
			http.Redirect(w, r, "/releases/tag/v0.3.6", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	for _, c := range []struct {
		env    []string
		argv   []string
		code   int
		stdout string
		stderr string
	}{
		{nil, []string{"self-update", "--help"}, 0, "Usage: rpak self-update [VERSION]", ""},
		{nil, []string{"-v", "self-update", "-h"}, 0, "Usage: rpak self-update [VERSION]", ""},
		{nil, []string{"self-update", "--now"}, 2, "", "rpak self-update: unknown option --now\n"},
		{[]string{"RPAK_TEST_VERSION=v0.3.5"}, []string{"self-update", "--check"}, 10, "rpak v0.3.5; the latest release v0.3.6 is newer: rpak self-update\n", ""},
		{[]string{"RPAK_TEST_VERSION=v0.3.6"}, []string{"self-update", "--check"}, 0, "rpak v0.3.6 is the latest release\n", ""},
		{[]string{"RPAK_TEST_VERSION=v0.3.6"}, []string{"self-update", "-y"}, 0, "rpak v0.3.6 is the latest release\n", ""},
		{[]string{"RPAK_TEST_VERSION=v0.3.5", "RPAK_RELEASE_BASE=" + srv.URL + "/none"}, []string{"self-update", "--check"}, 1, "", "rpak self-update: cannot find the latest release: HTTP 404"},
		// The test binary is a development build ("test").
		{nil, []string{"self-update", "-y"}, 1, "", "rpak self-update: rpak test is a development build, not a release; --force replaces it with one\n"},
		{[]string{"RPAK_TEST_SELF_UPDATE=disabled", "RPAK_TEST_VERSION=v0.3.5"}, []string{"self-update", "-y"}, 1, "", "rpak self-update: rpak was installed by a package manager; update it with that\n"},
		{[]string{"RPAK_TEST_SELF_UPDATE=disabled", "RPAK_TEST_VERSION=v0.3.5"}, []string{"self-update", "--check"}, 1, "", "rpak was installed by a package manager"},
	} {
		requests.Store(0)
		cmd := e.command(c.argv...)
		cmd.Env = append(append(cmd.Env, "RPAK_RELEASE_BASE="+srv.URL+"/releases"), c.env...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		cmd.Run()
		if code := cmd.ProcessState.ExitCode(); code != c.code || !strings.Contains(stdout.String(), c.stdout) || !strings.Contains(stderr.String(), c.stderr) {
			t.Errorf("%q %q: exit %d, stdout %q, stderr %q; want %d, %q, %q", c.env, c.argv, code, stdout.String(), stderr.String(), c.code, c.stdout, c.stderr)
		}
		if c.code == 2 || strings.Contains(c.stderr, "development build") || strings.Contains(c.stderr, "package manager") || strings.HasPrefix(c.stdout, "Usage") {
			if n := requests.Load(); n != 0 {
				t.Errorf("%q: %d requests", c.argv, n)
			}
		}
	}
	if e.read("calls") != "" {
		t.Error("flatpak was run")
	}
}
