package installation

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func env(vars map[string]string) Env {
	return Env{
		LookupEnv: func(name string) (string, bool) { v, ok := vars[name]; return v, ok },
		HomeDir:   func() (string, error) { return "/home/u", nil },
	}
}

func TestSystemDefault(t *testing.T) {
	if got := env(nil).SystemDefault().Path; got != "/var/lib/flatpak" {
		t.Errorf("default: %s", got)
	}
	if got := env(map[string]string{"FLATPAK_SYSTEM_DIR": ""}).SystemDefault().Path; got != "/var/lib/flatpak" {
		t.Errorf("empty FLATPAK_SYSTEM_DIR: %s", got)
	}
	if got := env(map[string]string{"FLATPAK_SYSTEM_DIR": "/srv/fp"}).SystemDefault().Path; got != "/srv/fp" {
		t.Errorf("FLATPAK_SYSTEM_DIR: %s", got)
	}
}

func writeConf(t *testing.T, dir, name, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "installations.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "installations.d", name), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestConfigured(t *testing.T) {
	dir := t.TempDir()
	writeConf(t, dir, "a.conf", `
[Installation "extra"]
Path=/opt/extra
DisplayName=Extra
Priority=5

[Installation "low"]
Path=/opt/low

[Installation "user"]
Path=/opt/bad-id

[Installation "nopath"]
DisplayName=x

[Installation noquotes]
Path=/opt/noquotes
`)
	writeConf(t, dir, "b.conf", `
[Installation "high"]
Path=/opt/high
Priority=10x

[Installation "extra"]
Path=/opt/duplicate
`)
	writeConf(t, dir, "c.conf.disabled", "[Installation \"off\"]\nPath=/opt/off\n")
	os.Symlink("a.conf", filepath.Join(dir, "installations.d", "link.txt"))

	got, warnings := env(map[string]string{"FLATPAK_CONFIG_DIR": dir}).Configured()
	var ids []string
	for _, inst := range got {
		ids = append(ids, inst.ID+"="+inst.Path)
	}
	if want := []string{"high=/opt/high", "extra=/opt/extra", "low=/opt/low"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("got %v, want %v", ids, want)
	}
	if len(warnings) != 3 {
		t.Errorf("warnings: %q", warnings)
	}

	// No installations.d at all
	if got, w := env(map[string]string{"FLATPAK_CONFIG_DIR": t.TempDir()}).Configured(); got != nil || w != nil {
		t.Errorf("empty config dir: %v %v", got, w)
	}
}

func TestSelect(t *testing.T) {
	dir := t.TempDir()
	writeConf(t, dir, "x.conf", "[Installation \"extra\"]\nPath=/opt/extra\n")
	e := env(map[string]string{"FLATPAK_CONFIG_DIR": dir, "FLATPAK_USER_DIR": "/u", "FLATPAK_SYSTEM_DIR": "/s"})
	for _, c := range []struct {
		options []string
		all     bool
		want    string
	}{
		{nil, false, "default=/s user=/u"},
		{nil, true, "default=/s user=/u extra=/opt/extra"},
		{[]string{"--user"}, true, "user=/u"},
		{[]string{"--system"}, true, "default=/s"},
		{[]string{"--user", "--system"}, true, "default=/s user=/u"},
		{[]string{"--installation=extra"}, true, "extra=/opt/extra"},
		{[]string{"--system", "--installation=default", "--installation=extra"}, true, "default=/s extra=/opt/extra"},
		{[]string{"--installation=default"}, false, "default=/s"},
	} {
		got, _, err := e.Select(c.options, c.all)
		if err != nil {
			t.Errorf("%v: %v", c.options, err)
			continue
		}
		var s []string
		for _, inst := range got {
			s = append(s, inst.ID+"="+inst.Path)
		}
		if strings.Join(s, " ") != c.want {
			t.Errorf("%v all=%v: got %v, want %s", c.options, c.all, s, c.want)
		}
	}
	if _, _, err := e.Select([]string{"--installation=nope"}, true); err == nil || !strings.Contains(err.Error(), "could not find installation nope") {
		t.Errorf("unknown installation: %v", err)
	}
}

func TestTitles(t *testing.T) {
	for inst, want := range map[Installation]string{
		{ID: "user", User: true}: "User installation",
		{ID: DefaultID}:          "System installation (default)",
		{ID: "extra"}:            "Installation system (extra)",
	} {
		if got := inst.Title(); got != want {
			t.Errorf("%s: %q, want %q", inst.ID, got, want)
		}
	}
}

func TestAuthenticatedRemotes(t *testing.T) {
	dir := t.TempDir()
	inst := Installation{ID: "user", Path: dir, User: true}
	if got, err := inst.AuthenticatedRemotes(); err != nil || got != nil {
		t.Fatalf("no repository: %q, %v", got, err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := `[core]
repo_version=1
mode=bare-user-only

[remote "flathub"]
url=https://dl.flathub.org/repo/
xa.title=Flathub

[remote "paid"]
url=https://example.com/repo/
xa.authenticator-name=com.example.Authenticator
xa.authenticator-install=true

[remote "empty"]
url=https://example.org/repo/
xa.authenticator-name=
`
	if err := os.WriteFile(filepath.Join(dir, "repo", "config"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := inst.AuthenticatedRemotes()
	if err != nil || !reflect.DeepEqual(got, []string{"paid"}) {
		t.Fatalf("got %q, %v; want [paid]", got, err)
	}
}
