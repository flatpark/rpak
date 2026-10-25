package app

import (
	"bytes"
	"strings"
	"testing"

	"github.com/flatpark/rpak/internal/clean"
	"github.com/flatpark/rpak/internal/installation"
)

func TestCommitLabel(t *testing.T) {
	for what, want := range map[string][2]string{
		"flathub:app/com.example.App/x86_64/stable":              {"com.example.App", "flathub"},
		"flathub:runtime/org.gnome.Platform/x86_64/47":           {"org.gnome.Platform//47", "flathub"},
		"deploy/app/com.example.App/x86_64/beta":                 {"com.example.App//beta", "deployed"},
		"app/com.example.App/x86_64/stable":                      {"com.example.App", ""},
		"0123456789ab":                                           {"commit 0123456789ab", "no ref"},
		"appstream2/x86_64":                                      {"appstream2/x86_64", ""},
		"flatpark:app/evil\x1b[2J/x86_64/stable":                 {"evil?[2J", "flatpark"},
		"remote:ostree/0/1/2":                                    {"ostree/0/1/2", "remote"},
		"0123456789AB":                                           {"0123456789AB", ""},
		"some/odd:name/with/a/colon/after/a/slash/like/this/one": {"some/odd:name/with/a/colon/after/a/slash/like/this/one", ""},
	} {
		label, note := commitLabel(what)
		if got := [2]string{sanitize(label), note}; got != want {
			t.Errorf("commitLabel(%q) = %q, want %q", what, got, want)
		}
	}
}

// cleanRun prints a report like cleanCommand does, for a terminal of cols
// (0: not a terminal).
func cleanRun(cols int, color, dryRun bool) string {
	var b bytes.Buffer
	t := term{}
	if cols > 0 {
		t = term{tty: true, redraw: true, color: color, cols: cols}
	}
	p := &cleanPrinter{w: &b, t: t, home: "/home/me", dryRun: dryRun}
	p.installation(installation.Installation{User: true, Path: "/home/me/.local/share/flatpak"})
	p.section(kindCommit, "Installers cached in commits")
	p.item(210_000_000, "flathub:app/com.vendor.enterprise.CollaborationSuite.Desktop/x86_64/stable")
	p.item(9_000_000, "deploy/app/com.sogou.微信输入法/x86_64/stable")
	p.item(4_096, "0123456789ab")
	p.section(kindPath, "Repository leftovers")
	p.section(kindPath, "Interrupted installs and updates") // empty: no subheading
	p.item(110_600, "/home/me/.local/share/flatpak/app/org.example.App/x86_64/stable/."+strings.Repeat("b", 64)+"-Xy12Zq")
	p.item(0, "/home/me/.local/share/flatpak/app/org.example.Gone")
	p.summary(false, "", 0)
	p.installation(installation.Installation{ID: installation.DefaultID, Path: "/var/lib/flatpak"})
	p.section(kindCommit, "Installers cached in commits")
	p.item(2_410_300_000, "flathub:app/com.example.Big/x86_64/stable")
	p.summary(true, `run "sudo rpak clean --system"`, 2_410_300_000)
	p.apps()
	p.app(clean.AppData{ID: "com.example.Old", Data: true, Size: 120_400_000, Override: true})
	p.app(clean.AppData{ID: "com.example.OnlyOverride", Override: true})
	p.kept(120_400_000)
	p.total(5, 219_114_696)
	return b.String()
}

func TestCleanOutput(t *testing.T) {
	// Not a terminal: whole lines.
	want := `User installation  ~/.local/share/flatpak
  Installers cached in commits
     210.0 MB  com.vendor.enterprise.CollaborationSuite.Desktop  flathub
       9.0 MB  com.sogou.微信输入法  deployed
       4.1 kB  commit 0123456789ab  no ref
  Interrupted installs and updates
     110.6 kB  app/org.example.App/x86_64/stable/.` + strings.Repeat("b", 64) + `-Xy12Zq
            -  app/org.example.Gone
  Freed 219.1 MB

System installation (default)  /var/lib/flatpak
  Installers cached in commits
       2.4 GB  com.example.Big  flathub
  Would free 2.4 GB; run "sudo rpak clean --system"

Data of apps that are no longer installed
     120.4 MB  com.example.Old  ~/.var/app, overrides
            -  com.example.OnlyOverride  overrides
  Kept 120.4 MB; "rpak clean --delete-data" deletes it

Freed 219.1 MB (5 items)
`
	if got := cleanRun(0, false, false); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}

	// 60 columns: labels shortened in the middle, notes dropped before a
	// label gets short; nothing wider than 59 cells.
	got := cleanRun(60, false, false)
	for _, l := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if w := textWidth(l); w > 59 {
			t.Errorf("%d cells: %q", w, l)
		}
	}
	for _, want := range []string{
		"     210.0 MB  com.vendor.enterprise.C…ite.Desktop  flathub\n",
		"       9.0 MB  com.sogou.微信输入法  deployed\n",
		"     110.6 kB  app/org.example.App/x86_64/st…bbbbbbb-Xy12Zq\n",
		// The commands to run fit.
		"  Would free 2.4 GB; run \"sudo rpak clean --system\"\n",
		"  Kept 120.4 MB; \"rpak clean --delete-data\" deletes it\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("60 columns: no %q in\n%s", want, got)
		}
	}
	// 30 columns: the notes go.
	got = cleanRun(30, false, false)
	if !strings.Contains(got, "     210.0 MB  com.vendo…ktop\n") || !strings.Contains(got, "       9.0 MB  com.sogou…入法\n") ||
		!strings.Contains(got, "     120.4 MB  com.examp….Old\n") {
		t.Errorf("30 columns:\n%s", got)
	}

	// Colours: bold headings, faint notes, yellow hint, green total; the
	// same text otherwise.
	colored := cleanRun(200, true, false)
	for _, want := range []string{
		"\x1b[1mUser installation\x1b[0m  \x1b[2m~/.local/share/flatpak\x1b[0m\n",
		"com.example.Big  \x1b[2mflathub\x1b[0m\n",
		"\x1b[33mWould free 2.4 GB;",
		"\x1b[2mKept 120.4 MB;",
		"\x1b[32mFreed 219.1 MB (5 items)\x1b[0m\n",
	} {
		if !strings.Contains(colored, want) {
			t.Errorf("no %q in %q", want, colored)
		}
	}
	if plain := sgr.ReplaceAllString(colored, ""); plain != cleanRun(200, false, false) {
		t.Errorf("with colours, the text differs:\n%s", plain)
	}
	if strings.Contains(cleanRun(0, false, false)+cleanRun(60, false, false), "\x1b") {
		t.Error("escape sequences without colours")
	}

	// A dry run says so, and its total is not green.
	dry := cleanRun(200, true, true)
	if !strings.Contains(dry, "  Would free 219.1 MB\n") || !strings.Contains(dry, "\nWould free 219.1 MB (5 items)\n") {
		t.Errorf("dry run:\n%s", dry)
	}
}

func TestCleanOutputNothing(t *testing.T) {
	for _, c := range []struct {
		dryRun bool
		want   string
	}{
		{false, "User installation  ~/.local/share/flatpak\n  Nothing to free\n\nNothing freed\n"},
		{true, "User installation  ~/.local/share/flatpak\n  Nothing to free\n\nNothing to free\n"},
	} {
		var b bytes.Buffer
		p := &cleanPrinter{w: &b, home: "/home/me", dryRun: c.dryRun}
		p.installation(installation.Installation{User: true, Path: "/home/me/.local/share/flatpak"})
		p.section(kindCommit, "Installers cached in commits")
		p.summary(false, "", 0)
		p.total(0, 0)
		if b.String() != c.want {
			t.Errorf("dry run %v: got\n%q, want\n%q", c.dryRun, b.String(), c.want)
		}
	}
	// No groups at all: just the total.
	var b bytes.Buffer
	(&cleanPrinter{w: &b}).total(0, 0)
	if b.String() != "Nothing freed\n" {
		t.Errorf("got %q", b.String())
	}
	// Items without size, an installation that failed.
	b.Reset()
	p := &cleanPrinter{w: &b}
	p.installation(installation.Installation{User: true, Path: "/u"})
	p.section(kindPath, "Repository leftovers")
	p.item(0, "/u/repo/refs: 1 empty directory")
	p.summary(false, "", 0)
	p.installation(installation.Installation{ID: installation.DefaultID, Path: "/s"})
	p.failed()
	p.total(1, 0)
	if want := "User installation  /u\n  Repository leftovers\n            -  repo/refs: 1 empty directory\n  Removed 1 item\n\n" +
		"System installation (default)  /s\n  Not cleaned\n\nRemoved 1 item\n"; b.String() != want {
		t.Errorf("got\n%s\nwant\n%s", b.String(), want)
	}
	p.dryRun = true
	if got := p.freedText(2, 0, true); got != "Would remove 2 items" {
		t.Errorf("got %q", got)
	}
	// A home of "/" is not abbreviated.
	if got := (&cleanPrinter{home: "/"}).tilde("/var/lib/flatpak"); got != "/var/lib/flatpak" {
		t.Errorf("got %q", got)
	}
	if got := (&cleanPrinter{home: "/home/me"}).tilde("/home/meow/x"); got != "/home/meow/x" {
		t.Errorf("got %q", got)
	}
}
