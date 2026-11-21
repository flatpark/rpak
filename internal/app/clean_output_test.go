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

// inUseRun prints an installation with an old deployment removed and two
// still in use, the second without a pid, then a total.
func inUseRun(cols int, color, dryRun bool) string {
	var b bytes.Buffer
	t := term{}
	if cols > 0 {
		t = term{tty: true, redraw: true, color: color, cols: cols}
	}
	base := "/home/me/.local/share/flatpak"
	p := &cleanPrinter{w: &b, t: t, home: "/home/me", dryRun: dryRun}
	p.installation(installation.Installation{User: true, Path: base})
	p.section(kindPath, "Old deployments no longer in use")
	p.item(12_300_000, base+"/.removed/org.example.Old-"+strings.Repeat("1", 64))
	p.section(kindPath, "rpak's prefetch directories") // no items: no subheading
	p.inUseSection([]clean.InUse{
		{Path: base + "/.removed/com.tencent.WeChat-2c45506193f71d4a6e00a3d418aa5deb14c09bbd3a7c4df35c8d067bcf47ee18", Size: 757_000_000, ID: "com.tencent.WeChat", PID: 1234},
		{Path: base + "/.removed/ai.usemagpie.Magpie-" + strings.Repeat("4", 64), Size: 93_000_000, ID: "ai.usemagpie.Magpie"},
	})
	p.summary(false, "", 0)
	p.total(1, 12_300_000)
	return b.String()
}

func TestCleanOutputInUse(t *testing.T) {
	// Not a terminal: whole lines. Listed after what was freed, not
	// counted, but told in the totals.
	want := `User installation  ~/.local/share/flatpak
  Old deployments no longer in use
      12.3 MB  .removed/org.example.Old-` + strings.Repeat("1", 64) + `
  Old deployments still in use
     757.0 MB  .removed/com.tencent.WeChat-2c45506193f71d4a6e00a3d418aa5deb14c09bbd3a7c4df35c8d067bcf47ee18  used by com.tencent.WeChat (pid 1234)
      93.0 MB  .removed/ai.usemagpie.Magpie-` + strings.Repeat("4", 64) + `  used by ai.usemagpie.Magpie
  Quit them and run rpak clean again
  Freed 12.3 MB; 850.0 MB still in use

Freed 12.3 MB (1 item); 850.0 MB still in use
`
	if got := inUseRun(0, false, false); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	dry := inUseRun(0, false, true)
	if !strings.Contains(dry, "  Old deployments still in use\n") || !strings.Contains(dry, "  used by com.tencent.WeChat (pid 1234)\n") ||
		!strings.HasSuffix(dry, "\n  Would free 12.3 MB; 850.0 MB still in use\n\nWould free 12.3 MB (1 item); 850.0 MB still in use\n") {
		t.Errorf("dry run:\n%s", dry)
	}

	// 100 columns: the directory shortened in the middle as other items
	// are, the note kept; nothing wider than 99 cells.
	got := inUseRun(100, false, false)
	for _, l := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if w := textWidth(l); w > 99 {
			t.Errorf("%d cells: %q", w, l)
		}
	}
	for _, want := range []string{
		"     757.0 MB  .removed/com.tencent.WeChat-2c…8d067bcf47ee18  used by com.tencent.WeChat (pid 1234)\n",
		"      93.0 MB  .removed/ai.usemagpie.Magpie-4444444…444444444444444444  used by ai.usemagpie.Magpie\n",
		"  Quit them and run rpak clean again\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("100 columns: no %q in\n%s", want, got)
		}
	}
	// Narrower: the note is what the section is for, so the directory gives
	// way first, then the pid, and only then the note.
	for _, c := range []struct {
		cols int
		want string
	}{
		{80, "     757.0 MB  .removed/com.ten…cf47ee18  used by com.tencent.WeChat (pid 1234)\n"},
		{60, "     757.0 MB  .removed/c…7ee18  used by com.tencent.WeChat\n"},
		{40, "     757.0 MB  .removed/com.ten…f47ee18\n"},
	} {
		if got := inUseRun(c.cols, false, false); !strings.Contains(got, c.want) {
			t.Errorf("%d columns: no %q in\n%s", c.cols, c.want, got)
		}
	}

	// Colours: faint notes and hint, as for the other items.
	colored := inUseRun(200, true, false)
	for _, want := range []string{
		"  \x1b[2mused by com.tencent.WeChat (pid 1234)\x1b[0m\n",
		"  \x1b[2mQuit them and run rpak clean again\x1b[0m\n",
		"\x1b[32mFreed 12.3 MB (1 item)\x1b[0m; 850.0 MB still in use\n",
	} {
		if !strings.Contains(colored, want) {
			t.Errorf("no %q in %q", want, colored)
		}
	}
	if plain := sgr.ReplaceAllString(colored, ""); plain != inUseRun(200, false, false) {
		t.Errorf("with colours, the text differs:\n%s", plain)
	}
}

// TestCleanOutputOnlyInUse: when nothing can be freed because everything
// is in use, the totals say so rather than just "Nothing to free"; with
// several installations, the hint ends each one's section.
func TestCleanOutputOnlyInUse(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		var b bytes.Buffer
		p := &cleanPrinter{w: &b, home: "/home/me", dryRun: dryRun}
		p.installation(installation.Installation{ID: installation.DefaultID, Path: "/var/lib/flatpak"})
		p.section(kindPath, "Old deployments no longer in use")
		p.inUseSection([]clean.InUse{{Path: "/var/lib/flatpak/.removed/org.example.Sys-" + strings.Repeat("a", 64), Size: 2_000_000, ID: "org.example.Sys", PID: 7}})
		p.summary(true, `run "sudo rpak clean --system"`, 0)
		p.installation(installation.Installation{User: true, Path: "/home/me/.local/share/flatpak"})
		p.inUseSection(nil) // nothing in use: nothing said
		p.summary(false, "", 0)
		p.installation(installation.Installation{ID: "extra", Path: "/opt/flatpak"})
		p.inUseSection([]clean.InUse{{Path: "/opt/flatpak/.removed/org.example.Extra-" + strings.Repeat("b", 64), Size: 1_000_000, ID: "org.example.Extra"}})
		p.summary(false, "", 0)
		p.total(0, 0)
		nothing := "Nothing freed"
		if dryRun {
			nothing = "Nothing to free"
		}
		want := "System installation (default)  /var/lib/flatpak\n" +
			"  Old deployments still in use\n" +
			"       2.0 MB  .removed/org.example.Sys-" + strings.Repeat("a", 64) + "  used by org.example.Sys (pid 7)\n" +
			"  Quit them and run rpak clean again\n" +
			"  Nothing to free; 2.0 MB still in use\n\n" +
			"User installation  ~/.local/share/flatpak\n" +
			"  Nothing to free\n\n" +
			"Installation system (extra)  /opt/flatpak\n" +
			"  Old deployments still in use\n" +
			"       1.0 MB  .removed/org.example.Extra-" + strings.Repeat("b", 64) + "  used by org.example.Extra\n" +
			"  Quit them and run rpak clean again\n" +
			"  Nothing to free; 1.0 MB still in use\n\n" +
			nothing + "; 3.0 MB still in use\n"
		if b.String() != want {
			t.Errorf("dry run %v: got\n%s\nwant\n%s", dryRun, b.String(), want)
		}
	}
}
