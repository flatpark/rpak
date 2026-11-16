package app

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/flatpark/rpak/internal/doctor"
)

const doctorUsage = `Usage: rpak doctor [APP_ID]...

Collects what is needed to look into a problem: rpak, flatpak and the
system, the environment variables that matter, the installations and
their remotes, the extra-data directory and whether the remotes can be
reached. With app ids, also each app: where it is installed, its runtime,
permissions and overrides, and whether its extra-data files can be
downloaded, those of the installed commit and of the remote's current one
(or, for an app that is not installed, of every remote that has it). What
looks wrong is marked, and summed up with what to do about it at the end.

When reporting a problem, paste the whole output into the issue. Home
directories and user names are replaced, passwords in proxy URLs hidden,
and the files shared with an app through the document portal are only
counted.

doctor only reads: it changes no files and takes no locks. It needs the
network: at most 15 seconds for a remote's summary and 10 for each other
check, all at once.

Exits with 1 if it found a problem marked ✗, else 0.

Options:
  -h, --help     show this help
`

// doctorCommand is `rpak doctor`.
func doctorCommand(version string, argv []string, stdout, stderr io.Writer) int {
	var apps []string
	dashes := false
	for _, arg := range argv {
		switch {
		case dashes:
			apps = append(apps, arg)
		case arg == "--":
			dashes = true
		case arg == "-h" || arg == "--help":
			fmt.Fprint(stdout, doctorUsage)
			return 0
		case strings.HasPrefix(arg, "-"):
			fmt.Fprintf(stderr, "rpak doctor: unknown option %s\nSee \"rpak doctor --help\".\n", arg)
			return 2
		default:
			apps = append(apps, arg)
		}
	}
	report := doctor.Run(context.Background(), doctor.NewConfig(version, flatpakPath(), apps))
	writeReport(stdout, detectTerm(stdout).color, report)
	if report.Failed() {
		return 1
	}
	return 0
}

// The marks of the report's items; a heading has none, a fact two spaces.
var doctorMarks = map[doctor.Level]seg{
	doctor.Info: {"  ", ""},
	doctor.OK:   {"✓ ", sgrGreen},
	doctor.Warn: {"! ", sgrYellow},
	doctor.Fail: {"✗ ", sgrRed},
}

// writeReport prints the report as plain text for an issue: sections, an
// item per line with blocks indented below it, and the problems with what
// to do about them at the end. Lines are not cut to the terminal: all of
// it is meant to be copied.
func writeReport(w io.Writer, color bool, r doctor.Report) {
	var lines []line
	add := func(l line) { lines = append(lines, l) }
	text := func(s string) line { return line{{s, ""}} }
	item := func(it doctor.Item) {
		add(line{doctorMarks[it.Level], {it.Text, ""}})
		if it.Block != "" {
			for _, b := range strings.Split(it.Block, "\n") {
				add(text(strings.TrimRight("      "+b, " \t")))
			}
		}
	}

	add(text("Paste all of this into the issue, between lines of three backticks (```)."))
	for _, s := range r.Sections {
		add(nil)
		add(line{{"== " + s.Title, sgrBold}})
		for _, it := range s.Items {
			if it.Head {
				add(line{{it.Text, sgrBold}})
				continue
			}
			item(it)
		}
	}

	add(nil)
	add(line{{"== Problems", sgrBold}})
	problems := r.Problems()
	if len(problems) == 0 {
		add(line{doctorMarks[doctor.OK], {"none found", ""}})
	}
	for _, p := range problems {
		ctx := ""
		if p.Context != "" {
			ctx = p.Context + ": "
		}
		add(line{doctorMarks[p.Level], {ctx + p.Text, ""}})
		if p.Hint != "" {
			add(line{{"    → " + p.Hint, sgrFaint}})
		}
	}

	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.render(color))
		b.WriteByte('\n')
	}
	io.WriteString(w, b.String())
}
