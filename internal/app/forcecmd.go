package app

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/flatpark/rpak/internal/args"
	fr "github.com/flatpark/rpak/internal/forceruntime"
	"github.com/flatpark/rpak/internal/plan"
	"github.com/flatpark/rpak/internal/prefetch"
)

const patchUsage = `Usage: rpak patch [--list]
       rpak patch --force-runtime[=TARGET] [-y] [--noninteractive] [-v] APP...
       rpak patch --reset[=PATCH] [-y] [--noninteractive] [-v] APP...

Experimental. Changes installed apps of the user installation in ways
their developers did not build them for, and remembers it: later
"rpak update" and "rpak install" keep the changes.

  --list            show the patched apps (the default)
  --force-runtime[=TARGET]
                    make the apps run on a newer branch of their runtime:
                    TARGET is latest (the default), a branch such as 26.08,
                    or a runtime ID/ARCH/BRANCH. The installed version of
                    the app is kept; "rpak update --force-runtime" also
                    updates it. An app with extra-data is reinstalled from
                    rpak's local remote rpak-local.
  --reset[=PATCH]   undo the app's patches, or only PATCH (force-runtime,
                    the only kind so far): forget them and reinstall the
                    app from its remote as built, with its runtime

The apps were not tested that way and may break.
`

// patchOptions are flatpak options rpak patch passes on.
var patchOptions = map[string]bool{"-y": true, "--assumeyes": true, "--noninteractive": true, "-v": true, "--verbose": true}

// patchCommand is `rpak patch`; globals are flatpak's global options
// before the command (-v), passed on to flatpak.
func patchCommand(version string, globals, argv []string, stdout, stderr io.Writer) int {
	var (
		opts, apps   []string
		list, reset  bool
		force        bool
		target, kind string
	)
	for i, a := range argv {
		name, value, hasValue := strings.Cut(a, "=")
		switch {
		case a == "--":
			apps = append(apps, argv[i+1:]...)
		case a == "-h" || a == "--help":
			fmt.Fprint(stdout, patchUsage)
			return 0
		case a == "--list":
			list = true
		case name == "--reset":
			reset, kind = true, value
			if hasValue && value != fr.Kind {
				fmt.Fprintf(stderr, "rpak: patch --reset=%s: the only kind of patch is %s\n", value, fr.Kind)
				return 2
			}
		case name == args.ForceRuntimeOption:
			force, target = true, value
		case a == "--user" || a == "-u":
			// The only installation supported.
		case patchOptions[a] || (len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.Trim(a[1:], "yv") == ""):
			opts = append(opts, a)
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(stderr, "rpak: patch: unknown option %s (only the user installation is supported)\n\n%s", a, patchUsage)
			return 2
		default:
			apps = append(apps, a)
		}
		if a == "--" {
			break
		}
	}
	modes := 0
	for _, m := range []bool{list, reset, force} {
		if m {
			modes++
		}
	}
	switch {
	case modes > 1:
		fmt.Fprintf(stderr, "rpak: patch: --list, --reset and --force-runtime go one at a time\n")
		return 2
	case modes == 0 || list:
		if len(apps) > 0 {
			fmt.Fprintf(stderr, "rpak: patch: give --force-runtime or --reset with the apps\n")
			return 2
		}
		return patchList(stdout, stderr)
	case len(apps) == 0:
		fmt.Fprintf(stderr, "rpak: patch: which apps? rpak patch %s APP...\n", map[bool]string{true: "--reset", false: "--force-runtime"}[reset])
		return 2
	case reset:
		_ = kind // every kind is force-runtime so far
		return patchReset(version, globals, opts, apps, stderr)
	}
	return patchForceRuntime(version, globals, opts, apps, target, stderr)
}

func patchList(stdout, stderr io.Writer) int {
	user, err := prefetch.DefaultUserBaseDir()
	if err != nil {
		fmt.Fprintf(stderr, "rpak: %v\n", err)
		return 1
	}
	if !fr.HasRecords(user) {
		fmt.Fprintln(stdout, "No app is patched.")
		return 0
	}
	unlock, err := lockForce(user)
	if err != nil {
		fmt.Fprintf(stderr, "rpak: %v\n", err)
		return 1
	}
	defer unlock()
	f := &forceFlow{ctx: context.Background(), user: user, stderr: stderr,
		query: plan.ExecRunner{Path: flatpakPath(), Timeout: forceQueryTimeout}}
	f.setRecords(loadRecords(user, stderr, true))
	f.dropStale()
	if len(f.records) == 0 {
		fmt.Fprintln(stdout, "No app is patched.")
		return 0
	}
	writeRecords(stdout, f.records)
	return 0
}

// writeRecords prints the records as a table.
func writeRecords(w io.Writer, records []fr.Record) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "App\tPatch\tBuilt for\tRuns on\tMethod\tRemote\tTarget")
	for _, r := range records {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", strings.TrimPrefix(r.Ref, "app/"), fr.Kind, r.Original, r.Runtime, r.Method, r.Remote, targetText(r.Target))
	}
	tw.Flush()
}

func targetText(t string) string {
	if t == "" {
		return fr.Latest
	}
	return t
}

// resetCmd is a flatpak install that undoes forcing.
type resetCmd struct {
	records []fr.Record
	argv    []string
}

// resetCommands are the flatpak commands that reinstall the records' apps
// from their upstream remotes, one per remote, in record order:
// install --user --reinstall [OPTS] REMOTE REF...
func resetCommands(records []fr.Record, opts []string) []resetCmd {
	var out []resetCmd
	byRemote := map[string]int{}
	for _, r := range records {
		i, ok := byRemote[r.Remote]
		if !ok {
			i = len(out)
			byRemote[r.Remote] = i
			argv := append([]string{"install", "--user", "--reinstall"}, opts...)
			out = append(out, resetCmd{argv: append(argv, r.Remote)})
		}
		out[i].records = append(out[i].records, r)
		out[i].argv = append(out[i].argv, r.Ref)
	}
	return out
}

func patchReset(version string, globals, opts, apps []string, stderr io.Writer) int {
	user, err := prefetch.DefaultUserBaseDir()
	if err != nil {
		fmt.Fprintf(stderr, "rpak: %v\n", err)
		return 1
	}
	f := &forceFlow{ctx: context.Background(), version: version, user: user, stderr: stderr, globals: globals,
		flatpak: flatpakPath(), branches: map[string][]string{}, dryRun: dryRunEnv()}
	if !f.dryRun {
		unlock, err := lockForce(user)
		if err != nil {
			fmt.Fprintf(stderr, "rpak: %v\n", err)
			return 1
		}
		defer unlock()
	}
	f.query = plan.ExecRunner{Path: f.flatpak, Timeout: forceQueryTimeout}
	f.setRecords(loadRecords(user, stderr, !f.dryRun))
	f.dropStale()
	var selected []fr.Record
	for _, app := range apps {
		found := false
		for _, r := range f.records {
			if plan.Matches(r.Ref, app) {
				if _, dup := fr.Find(selected, r.Ref); !dup {
					selected = append(selected, r)
				}
				found = true
			}
		}
		if !found {
			fmt.Fprintf(stderr, "rpak: %s is not patched\n", app)
			return 1
		}
	}
	code := 0
	var unexported []string
	for _, c := range resetCommands(selected, opts) {
		argv := append(append([]string(nil), globals...), c.argv...)
		if f.dryRun {
			f.dryCmd(argv)
		} else if c := runAttached(f.flatpak, argv); c != 0 {
			code = c
			continue
		}
		for _, r := range c.records {
			f.records = fr.Remove(f.records, r.Ref)
			if r.Method == fr.MethodReexport {
				unexported = append(unexported, r.Ref)
			}
			if f.dryRun {
				f.dry("make %s run on %s again", idOf(r.Ref), r.Original)
			} else {
				fmt.Fprintf(stderr, "rpak: %s runs on %s again\n", idOf(r.Ref), r.Original)
			}
		}
	}
	f.save()
	if len(unexported) > 0 {
		f.unexport(unexported)
	}
	return code
}
