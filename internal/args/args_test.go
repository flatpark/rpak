package args

import (
	"reflect"
	"testing"
)

func TestValueOptionsBothForms(t *testing.T) {
	p := Parse("update", []string{
		"--arch=aarch64", "--subpath", "/lib", "--subpath=/share",
		"--installation", "extra", "--sideload-repo=/mnt/usb", "org.example.App",
	})
	if p.Arch != "aarch64" {
		t.Errorf("arch = %q", p.Arch)
	}
	// "/lib" is the value of --subpath, not a positional argument.
	if want := []string{"org.example.App"}; !reflect.DeepEqual(p.Positionals, want) {
		t.Errorf("positionals = %q, want %q", p.Positionals, want)
	}
	if want := []string{"--installation=extra"}; !reflect.DeepEqual(p.Installations, want) {
		t.Errorf("installations = %q, want %q", p.Installations, want)
	}
	if len(p.Unknown) != 0 {
		t.Errorf("unknown = %q", p.Unknown)
	}

	p = Parse("update", []string{"--arch", "x86_64", "app"})
	if p.Arch != "x86_64" || !reflect.DeepEqual(p.Positionals, []string{"app"}) {
		t.Errorf("separate value form: %#v", p)
	}

	// --gpg-file only takes a value for install.
	p = Parse("install", []string{"--gpg-file", "key.gpg", "remote", "app"})
	if want := []string{"remote", "app"}; !reflect.DeepEqual(p.Positionals, want) {
		t.Errorf("install positionals = %q, want %q", p.Positionals, want)
	}
}

func TestShortOptions(t *testing.T) {
	p := Parse("update", []string{"-uy", "-vv", "org.example.App"})
	if !p.ExplicitInstallation || !reflect.DeepEqual(p.Installations, []string{User}) {
		t.Errorf("installations = %q", p.Installations)
	}
	if !reflect.DeepEqual(p.Positionals, []string{"org.example.App"}) || len(p.Unknown) != 0 {
		t.Errorf("parsed = %#v", p)
	}

	p = Parse("install", []string{"-yx", "-", "app"})
	if !reflect.DeepEqual(p.Unknown, []string{"-x"}) {
		t.Errorf("unknown = %q", p.Unknown)
	}
	// A lone "-" is a positional argument for GLib.
	if !reflect.DeepEqual(p.Positionals, []string{"-", "app"}) {
		t.Errorf("positionals = %q", p.Positionals)
	}
}

func TestDoubleDash(t *testing.T) {
	p := Parse("install", []string{"-y", "flathub", "--", "--user", "-u", "--arch=x"})
	if p.ExplicitInstallation || p.Arch != "" {
		t.Errorf("options after -- were parsed: %#v", p)
	}
	if want := []string{"flathub", "--user", "-u", "--arch=x"}; !reflect.DeepEqual(p.Positionals, want) {
		t.Errorf("positionals = %q, want %q", p.Positionals, want)
	}
}

func TestOptionsAfterPositionals(t *testing.T) {
	p := Parse("install", []string{"flatpark", "com.google.Chrome", "--user", "-y"})
	if !reflect.DeepEqual(p.Positionals, []string{"flatpark", "com.google.Chrome"}) ||
		!reflect.DeepEqual(p.Installations, []string{User}) {
		t.Errorf("parsed = %#v", p)
	}
}

func TestInstallations(t *testing.T) {
	cases := []struct {
		argv []string
		want []string
		expl bool
	}{
		{nil, []string{User, System}, false},
		{[]string{"--user"}, []string{User}, true},
		{[]string{"--system"}, []string{System}, true},
		{[]string{"--user", "--system", "-u"}, []string{User, System}, true},
		{[]string{"--installation=a", "--installation", "b", "--installation=a"}, []string{"--installation=a", "--installation=b"}, true},
	}
	for _, c := range cases {
		p := Parse("update", c.argv)
		if !reflect.DeepEqual(p.Installations, c.want) || p.ExplicitInstallation != c.expl {
			t.Errorf("%q: installations = %q (explicit %v), want %q (%v)", c.argv, p.Installations, p.ExplicitInstallation, c.want, c.expl)
		}
	}
}

func TestUnknownOptionIsAFlag(t *testing.T) {
	p := Parse("update", []string{"--future-flag", "app", "--future-flag"})
	if !reflect.DeepEqual(p.Unknown, []string{"--future-flag"}) {
		t.Errorf("unknown = %q", p.Unknown)
	}
	if !reflect.DeepEqual(p.Positionals, []string{"app"}) || p.SkipReason != "" {
		t.Errorf("parsed = %#v", p)
	}
	// Options of the other command are unknown here.
	if p := Parse("install", []string{"--force-remove"}); len(p.Unknown) != 1 {
		t.Errorf("install --force-remove: unknown = %q", p.Unknown)
	}
}

func TestSkipOptions(t *testing.T) {
	cases := []struct {
		command string
		argv    []string
	}{
		{"update", []string{"--no-pull"}},
		{"update", []string{"--commit=abc"}},
		{"update", []string{"--commit", "abc", "app"}},
		{"update", []string{"--appstream"}},
		{"update", []string{"--sideload-repo", "/mnt"}},
		{"install", []string{"--sideload-repo=/mnt", "flathub", "app"}},
		{"install", []string{"--bundle", "app.flatpak"}},
		{"install", []string{"--from", "x"}},
		{"install", []string{"--image", "x"}},
		{"install", []string{"--no-pull", "flathub", "app"}},
	}
	for _, c := range cases {
		if p := Parse(c.command, c.argv); p.SkipReason == "" {
			t.Errorf("%s %q: not skipped", c.command, c.argv)
		}
	}
	// --commit's value must not become a positional either.
	if p := Parse("update", []string{"--commit", "abc", "app"}); !reflect.DeepEqual(p.Positionals, []string{"app"}) {
		t.Errorf("positionals = %q", p.Positionals)
	}
}

func TestSkipInstallLocations(t *testing.T) {
	for _, loc := range []string{
		"/tmp/app.flatpak", "./repo", "../repo", "http://example.com/x.flatpakref",
		"https://example.com/repo", "file:///srv/repo", "oci:/srv/img", "docker:x",
		"org.example.App.flatpakref", "bundle.flatpak",
	} {
		if p := Parse("install", []string{"-y", loc}); p.SkipReason == "" {
			t.Errorf("%q: not skipped", loc)
		}
	}
	for _, argv := range [][]string{
		{"flatpark", "com.google.Chrome"},
		{"com.google.Chrome//stable"},
		{"app/com.google.Chrome/x86_64/stable"},
	} {
		if p := Parse("install", argv); p.SkipReason != "" {
			t.Errorf("%q: skipped (%s)", argv, p.SkipReason)
		}
	}
	// update has no LOCATION argument.
	if p := Parse("update", []string{"./x"}); p.SkipReason != "" {
		t.Errorf("update ./x skipped")
	}
}

func TestSplitInstallRemote(t *testing.T) {
	remotes := map[string]bool{"flathub": true, "flatpark": true}
	isRemote := func(name string) bool { return remotes[name] }
	cases := []struct {
		positionals []string
		remote      string
		refs        []string
	}{
		{[]string{"flatpark", "com.google.Chrome"}, "flatpark", []string{"com.google.Chrome"}},
		{[]string{"flatpark", "a", "b"}, "flatpark", []string{"a", "b"}},
		{[]string{"com.google.Chrome", "org.example.App"}, "", []string{"com.google.Chrome", "org.example.App"}},
		// A single argument is a REF even if a remote has that name.
		{[]string{"flathub"}, "", []string{"flathub"}},
		{nil, "", nil},
	}
	for _, c := range cases {
		remote, refs := SplitInstallRemote(c.positionals, isRemote)
		if remote != c.remote || !reflect.DeepEqual(refs, c.refs) {
			t.Errorf("%q: got %q %q, want %q %q", c.positionals, remote, refs, c.remote, c.refs)
		}
	}
}
