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
system, the environment variables that matter, the graphics cards, the
installations and their remotes, the extra-data directory and how the
remotes answer. With app ids, also each app: where it is installed, its
runtime, permissions and overrides, and how the servers of its extra-data
files answer, for the installed commit and the remote's current one (or,
for an app that is not installed, for every remote that has it).

It reports what it finds and measures, and judges none of it: what the
report means is for the maintainers to read. When reporting a problem,
paste the whole output into the issue. Home directories and user names
are replaced, passwords in proxy URLs hidden, and the files shared with an
app through the document portal are only counted.

doctor only reads: it changes no files and takes no locks. It needs the
network: at most 15 seconds for a remote's summary and 10 for each other
check, all at once.

Exits with 0 once it has collected the report, whatever it found.

Options:
  -h, --help     show this help
`

// doctorCommand is `rpak doctor`.
func doctorCommand(version string, selfUpdate bool, argv []string, stdout, stderr io.Writer) int {
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
	report := doctor.Run(context.Background(), doctor.NewConfig(version, selfUpdate, flatpakPath(), apps))
	if err := writeReport(stdout, report); err != nil {
		fmt.Fprintf(stderr, "rpak doctor: %v\n", err)
		return 1
	}
	return 0
}

// writeReport prints the report as plain text for an issue: sections, an
// item per line, indented, with blocks indented further below it. There is
// no colour, and lines are not cut to the terminal: all of it is meant to
// be copied.
func writeReport(w io.Writer, r doctor.Report) error {
	var b strings.Builder
	b.WriteString("Paste all of this into the issue, between lines of three backticks (```).\n")
	for _, s := range r.Sections {
		b.WriteString("\n== " + s.Title + "\n")
		for _, it := range s.Items {
			if it.Head {
				b.WriteString(it.Text + "\n")
				continue
			}
			b.WriteString("  " + it.Text + "\n")
			if it.Block != "" {
				for _, l := range strings.Split(it.Block, "\n") {
					b.WriteString(strings.TrimRight("      "+l, " \t") + "\n")
				}
			}
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}
