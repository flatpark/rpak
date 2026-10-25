package app

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/flatpark/rpak/internal/clean"
	"github.com/flatpark/rpak/internal/installation"
	"github.com/flatpark/rpak/internal/metadata"
)

// The report of rpak clean, on stdout: a heading per installation, the
// items removed (or that would be) grouped by kind under a subheading, a
// line per installation with what it freed when there are several, the
// data of uninstalled apps, and a total. Items are printed as they are
// found, so the size column has a fixed width. On a terminal lines are cut
// to its width, in colour as for install and update (see output.go);
// otherwise every item is a whole line for logs and grep:
//
//	User installation  ~/.local/share/flatpak
//	  Installers cached in commits
//	     10.7 MB  app.audionaut.Audionaut  deployed
//	  Repository leftovers
//	           -  repo/refs: 4 empty directories
//
//	Freed 10.7 MB (2 items)

// cleanKind is what the items of a section are, which decides how they
// are shown.
type cleanKind int

const (
	kindCommit cleanKind = iota // a ref, or the start of a checksum
	kindPath                    // a path, shown relative to the installation
)

const (
	cleanItemIndent = 4
	cleanSizeWidth  = 9 // "999 bytes", the widest humanSize up to "999.9 PB"
	cleanGap        = 2
)

// cleanPrinter writes the report.
type cleanPrinter struct {
	w      io.Writer
	t      term
	home   string // abbreviated to "~"
	dryRun bool

	groups int    // headings printed
	base   string // path of the current installation
	kind   cleanKind
	title  string // subheading, printed before its first item
	items  int    // in the current installation
	freed  int64  // by the current installation
}

func newCleanPrinter(w io.Writer, dryRun bool) *cleanPrinter {
	home, _ := os.UserHomeDir()
	return &cleanPrinter{w: w, t: detectTerm(w), home: home, dryRun: dryRun}
}

func (p *cleanPrinter) write(lines ...line) { writeLines(p.w, p.t, lines) }

// tilde abbreviates the home directory in path.
func (p *cleanPrinter) tilde(path string) string {
	if p.home != "" && p.home != "/" {
		if rest, ok := strings.CutPrefix(path, p.home); ok && (rest == "" || rest[0] == '/') {
			return "~" + rest
		}
	}
	return path
}

// group starts a top-level group, after a blank line if it is not the
// first: a bold title and a faint note.
func (p *cleanPrinter) group(title, note string) {
	if p.groups > 0 {
		p.write(nil)
	}
	p.groups++
	l := line{{sanitize(title), sgrBold}}
	if note != "" {
		l.add(strings.Repeat(" ", cleanGap), "")
		l.add(sanitize(note), sgrFaint)
	}
	p.write(l)
	p.title, p.items, p.freed = "", 0, 0
}

func (p *cleanPrinter) installation(inst installation.Installation) {
	p.group(inst.Title(), p.tilde(inst.Path))
	p.base = inst.Path
}

// section sets the subheading and the kind of the items that follow.
func (p *cleanPrinter) section(kind cleanKind, title string) {
	p.kind, p.title = kind, title
}

// item is clean.Cleaner.Item: a removed thing, as found.
func (p *cleanPrinter) item(size int64, what string) {
	if p.title != "" {
		p.write(line{{"  " + p.title, ""}})
		p.title = ""
	}
	p.items++
	p.freed += size
	var label, note string
	switch p.kind {
	case kindCommit:
		label, note = commitLabel(what)
	default:
		label = what
		if rest, ok := strings.CutPrefix(what, p.base+"/"); ok && p.base != "" {
			label = rest
		}
		label = p.tilde(label)
	}
	p.write(p.itemLine(size, sanitize(label), sanitize(note)))
}

// itemLine is "    SIZE  LABEL  NOTE", the label shortened in the middle to
// fit the terminal, and the note dropped before the label gets short.
func (p *cleanPrinter) itemLine(size int64, label, note string) line {
	sizeText := "-"
	if size > 0 {
		sizeText = humanSize(size)
	}
	l := line{{strings.Repeat(" ", cleanItemIndent) + padLeft(sizeText, cleanSizeWidth) + strings.Repeat(" ", cleanGap), ""}}
	if width := p.t.width(); width > 0 {
		room := width - l.width()
		if note != "" && room-cleanGap-textWidth(note) < min(textWidth(label), 24) {
			note = ""
		}
		if note != "" {
			room -= cleanGap + textWidth(note)
		}
		label = truncateMiddle(label, max(room, 1))
	}
	l.add(label, "")
	if note != "" {
		l.add(strings.Repeat(" ", cleanGap), "")
		l.add(note, sgrFaint)
	}
	return l
}

// commitLabel shows the ref of a commit whose installers were dropped as
// the item lines of the install plan show apps, with where the ref is
// from: "flathub:app/org.x.Y/x86_64/stable" is "org.x.Y" from "flathub",
// "deploy/app/org.x.Y/x86_64/stable" (the ref flatpak keeps for the
// deployed commit) is "org.x.Y", "deployed". A commit without a ref is
// shown by the start of its checksum.
func commitLabel(what string) (label, note string) {
	if len(what) == 12 && metadata.ValidChecksum(what+strings.Repeat("0", 52)) {
		return "commit " + what, "no ref"
	}
	ref := what
	if remote, rest, ok := strings.Cut(what, ":"); ok && !strings.Contains(remote, "/") {
		ref, note = rest, remote
	} else if rest, ok := strings.CutPrefix(what, "deploy/"); ok {
		ref, note = rest, "deployed"
	}
	if strings.HasPrefix(ref, "app/") || strings.HasPrefix(ref, "runtime/") {
		return appID(ref), note
	}
	return ref, note
}

// summary ends an installation with what it freed: faint when nothing,
// yellow when it could only be inspected.
func (p *cleanPrinter) summary(readOnly bool, hint string, freed int64) {
	switch {
	case readOnly && freed > 0:
		p.write(line{{"  ", ""}, {fmt.Sprintf("Would free %s; %s", humanSize(freed), hint), sgrYellow}})
	case p.items == 0:
		p.write(line{{"  ", ""}, {"Nothing to free", sgrFaint}})
	default:
		p.write(line{{"  ", ""}, {p.freedText(p.items, p.freed, false), ""}})
	}
}

// failed ends an installation that could not be cleaned; the error is on
// stderr.
func (p *cleanPrinter) failed() {
	p.write(line{{"  ", ""}, {"Not cleaned", sgrRed}})
}

// freedText is "Freed 12.3 MB", with the count of items if count is set,
// or "Removed 2 items" for items that took no space, such as empty
// directories.
func (p *cleanPrinter) freedText(items int, freed int64, count bool) string {
	noun := "items"
	if items == 1 {
		noun = "item"
	}
	if freed == 0 {
		if p.dryRun {
			return fmt.Sprintf("Would remove %d %s", items, noun)
		}
		return fmt.Sprintf("Removed %d %s", items, noun)
	}
	verb := "Freed"
	if p.dryRun {
		verb = "Would free"
	}
	if count {
		return fmt.Sprintf("%s %s (%d %s)", verb, humanSize(freed), items, noun)
	}
	return fmt.Sprintf("%s %s", verb, humanSize(freed))
}

// apps starts the group of the data of apps that are no longer installed.
func (p *cleanPrinter) apps() {
	p.group("Data of apps that are no longer installed", "")
	p.base = ""
}

// app is a line of that group: "    SIZE  ID  ~/.var/app, overrides".
func (p *cleanPrinter) app(a clean.AppData) {
	var where []string
	if a.Data {
		where = append(where, "~/.var/app")
	}
	if a.Override {
		where = append(where, "overrides")
	}
	p.items++
	p.freed += a.Size
	p.write(p.itemLine(a.Size, sanitize(a.ID), strings.Join(where, ", ")))
}

// kept ends the group when the data was only listed.
func (p *cleanPrinter) kept(total int64) {
	p.write(line{{"  ", ""}, {fmt.Sprintf("Kept %s; \"rpak clean --delete-data\" deletes it", humanSize(total)), sgrFaint}})
}

// deleted ends the group when the data was deleted (or would be).
func (p *cleanPrinter) deleted() {
	verb := "Deleted"
	if p.dryRun {
		verb = "Would delete"
	}
	what := humanSize(p.freed)
	if p.freed == 0 {
		what = fmt.Sprintf("the data of %d apps", p.items)
		if p.items == 1 {
			what = "the data of 1 app"
		}
	}
	p.write(line{{"  ", ""}, {verb + " " + what, ""}})
}

// total is the last line, after a blank one: "Freed 132.8 MB (7 items)",
// green when something was removed.
func (p *cleanPrinter) total(items int, freed int64) {
	if p.groups > 0 {
		p.write(nil)
	}
	if items == 0 {
		text := "Nothing freed"
		if p.dryRun {
			text = "Nothing to free"
		}
		p.write(line{{text, ""}})
		return
	}
	sgr := sgrGreen
	if p.dryRun {
		sgr = ""
	}
	p.write(line{{p.freedText(items, freed, true), sgr}})
}
