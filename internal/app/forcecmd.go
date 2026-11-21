package app

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/flatpark/rpak/internal/args"
	fr "github.com/flatpark/rpak/internal/forceruntime"
	"github.com/flatpark/rpak/internal/installation"
	"github.com/flatpark/rpak/internal/plan"
	"github.com/flatpark/rpak/internal/prefetch"
)

const patchUsage = `Usage: rpak patch [--list]
       rpak patch --force-runtime[=TARGET] [-y] [--noninteractive] [-v] APP...
       rpak patch --extra-data=[NAME=]URL... [--force-runtime[=TARGET]] [-y] [-v] APP
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
  --extra-data=[NAME=]URL
                    download the extra-data file NAME of the app from URL
                    instead (a vendor's newer release, a link that works):
                    rpak downloads it, takes its checksum and size, and
                    reinstalls the app from rpak-local. Without NAME, the
                    app's only extra-data, or the one whose URL is clearly
                    the closest to URL. Give it again for other files. It
                    is kept until upstream changes that file itself.
  --reset[=PATCH]   undo the app's patches, or only PATCH (force-runtime
                    or extra-data); without any left the app is
                    reinstalled from its remote as built

The apps were not tested that way and may break; files given with
--extra-data are checked by nobody but you.
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
		extra        []string
	)
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		name, value, hasValue := strings.Cut(a, "=")
		switch {
		case a == "--":
			apps = append(apps, argv[i+1:]...)
			i = len(argv)
			continue
		case a == "-h" || a == "--help":
			fmt.Fprint(stdout, patchUsage)
			return 0
		case a == "--list":
			list = true
		case name == "--reset":
			reset, kind = true, value
			if hasValue && value != fr.Kind && value != fr.ExtraKind {
				fmt.Fprintf(stderr, "rpak: patch --reset=%s: the kinds of patch are %s and %s\n", value, fr.Kind, fr.ExtraKind)
				return 2
			}
		case name == args.ForceRuntimeOption:
			force, target = true, value
		case name == args.ExtraDataOption:
			if !hasValue {
				if i+1 == len(argv) {
					fmt.Fprintf(stderr, "rpak: patch: %s needs [NAME=]URL\n", args.ExtraDataOption)
					return 2
				}
				i++
				value = argv[i]
			}
			extra = append(extra, value)
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
	}
	modes := 0
	for _, m := range []bool{list, reset, force || len(extra) > 0} {
		if m {
			modes++
		}
	}
	switch {
	case modes > 1:
		fmt.Fprintf(stderr, "rpak: patch: --list, --reset and the patches (--force-runtime, --extra-data) go one at a time\n")
		return 2
	case modes == 0 || list:
		if len(apps) > 0 {
			fmt.Fprintf(stderr, "rpak: patch: give --force-runtime, --extra-data or --reset with the apps\n")
			return 2
		}
		return patchList(stdout, stderr)
	case len(apps) == 0:
		what := "--force-runtime"
		switch {
		case reset:
			what = "--reset"
		case len(extra) > 0:
			what = args.ExtraDataOption + "=URL"
		}
		fmt.Fprintf(stderr, "rpak: patch: which apps? rpak patch %s APP...\n", what)
		return 2
	case reset:
		return patchReset(version, globals, opts, apps, kind, stderr)
	}
	return patchApps(version, globals, opts, apps, force, target, extra, stderr)
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
	if !f.hasPatches() {
		fmt.Fprintln(stdout, "No app is patched.")
		return 0
	}
	if len(f.records) > 0 {
		writeRecords(stdout, f.records)
	}
	if len(f.extras) > 0 {
		if len(f.records) > 0 {
			fmt.Fprintln(stdout)
		}
		writeExtraRecords(stdout, f.extras)
	}
	return 0
}

// writeExtraRecords prints the extra-data records as a table: a line per
// replaced source, with the file it is downloaded from now and upstream's.
func writeExtraRecords(w io.Writer, records []fr.ExtraRecord) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "App\tPatch\tName\tURL\tUpstream's URL\tRemote")
	for _, r := range records {
		for _, s := range r.Sources {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", strings.TrimPrefix(r.Ref, "app/"), fr.ExtraKind, s.Name, s.URL, s.Base.URL, r.Remote)
		}
	}
	tw.Flush()
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

// patchReset is rpak patch --reset[=KIND] APP...: kind "" undoes every
// patch of the apps. An app left without patches is reinstalled from its
// upstream remote, as built; one left with a patch of the other kind is
// reexported with that one alone, from the upstream commit it has.
func patchReset(version string, globals, opts, apps []string, kind string, stderr io.Writer) int {
	user, err := prefetch.DefaultUserBaseDir()
	if err != nil {
		fmt.Fprintf(stderr, "rpak: %v\n", err)
		return 1
	}
	f := &forceFlow{ctx: context.Background(), version: version, user: user, stderr: stderr, globals: globals,
		flatpak: flatpakPath(), branches: map[string][]string{}, dryRun: dryRunEnv(),
		parsed: args.Parsed{Command: "patch", Installations: []string{args.User}, ExplicitInstallation: true}}
	if !f.dryRun {
		unlock, err := lockForce(user)
		if err != nil {
			fmt.Fprintf(stderr, "rpak: %v\n", err)
			return 1
		}
		defer unlock()
	}
	f.query = plan.ExecRunner{Path: f.flatpak, Timeout: forceQueryTimeout}
	f.options = append(append([]string(nil), opts...), args.User)
	// Where runtimes may be installed: a partial reset reexports and
	// checks them (fillRuntimes).
	for _, inst := range (installation.Env{}).All() {
		f.insts = append(f.insts, inst.Path)
	}
	f.setRecords(loadRecords(user, stderr, !f.dryRun))
	f.dropStale()
	resetRT := kind == "" || kind == fr.Kind
	resetExtra := kind == "" || kind == fr.ExtraKind
	var full []fr.Record // apps left without patches
	var partial []string // apps left with one
	for _, app := range apps {
		found := false
		for _, ref := range f.patchedRefs() {
			if !plan.Matches(ref, app) {
				continue
			}
			rt, hasRT := fr.Find(f.records, ref)
			ex, hasEx := fr.FindExtra(f.extras, ref)
			if !(hasRT && resetRT) && !(hasEx && resetExtra) {
				continue
			}
			found = true
			if (hasRT && !resetRT) || (hasEx && !resetExtra) {
				if !slices.Contains(partial, ref) {
					partial = append(partial, ref)
				}
				continue
			}
			if !hasRT {
				rt = fr.Record{Ref: ref, Remote: ex.Remote, Method: fr.MethodReexport}
			}
			if _, dup := fr.Find(full, ref); !dup {
				full = append(full, rt)
			}
		}
		if !found {
			what := "patched"
			if kind != "" {
				what = "patched with " + kind
			}
			fmt.Fprintf(stderr, "rpak: %s is not %s\n", app, what)
			return 1
		}
	}
	code := 0
	if len(partial) > 0 {
		code = f.resetPartial(partial, resetRT, resetExtra)
	}
	var unexported []string
	for _, c := range resetCommands(full, opts) {
		argv := append(append([]string(nil), globals...), c.argv...)
		if f.dryRun {
			f.dryCmd(argv)
		} else if c := runAttached(f.flatpak, argv); c != 0 {
			code = c
			continue
		}
		for _, r := range c.records {
			_, hadExtra := fr.FindExtra(f.extras, r.Ref)
			f.records = fr.Remove(f.records, r.Ref)
			f.extras = fr.RemoveExtra(f.extras, r.Ref)
			if r.Method == fr.MethodReexport || hadExtra {
				unexported = append(unexported, r.Ref)
			}
			switch {
			case r.Original == "" && f.dryRun:
				f.dry("make %s come from %s again", idOf(r.Ref), r.Remote)
			case r.Original == "":
				fmt.Fprintf(stderr, "rpak: %s comes from %s again\n", idOf(r.Ref), r.Remote)
			case f.dryRun:
				f.dry("make %s run on %s again", idOf(r.Ref), r.Original)
			default:
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

// patchedRefs lists the apps with a patch of either kind, sorted.
func (f *forceFlow) patchedRefs() []string {
	var refs []string
	for _, r := range f.records {
		refs = append(refs, r.Ref)
	}
	for _, r := range f.extras {
		if !slices.Contains(refs, r.Ref) {
			refs = append(refs, r.Ref)
		}
	}
	slices.Sort(refs)
	return refs
}

// resetPartial undoes one kind of patch of apps that keep the other: they
// are reexported, from the upstream commit they have, with what is left,
// and updated from rpak-local; an app not from rpak-local now only loses
// the record.
func (f *forceFlow) resetPartial(refs []string, resetRT, resetExtra bool) int {
	var items []item
	for _, ref := range refs {
		rt, hasRT := fr.Find(f.records, ref)
		if resetRT {
			f.records = fr.Remove(f.records, ref)
			hasRT = false
		}
		if resetExtra {
			f.extras = fr.RemoveExtra(f.extras, ref)
		}
		d, ok, _ := fr.Active(f.user, ref)
		if !ok || d.Origin != fr.RemoteName {
			continue
		}
		info, upstream, err := f.installedInfo(ref)
		if err != nil {
			return f.fail(1, "%s: %v", ref, err)
		}
		var it item
		if hasRT {
			if it, _, err = f.newItem(idOf(ref), upstream, info, rt.Target); err == nil {
				it.runtimePatch = true
			}
		} else {
			it, err = f.plainItem(idOf(ref), upstream, info)
		}
		if err != nil {
			return f.fail(1, "%s: %v", ref, err)
		}
		it.method = fr.MethodReexport
		items = append(items, it)
	}
	if len(items) == 0 {
		return 0
	}
	return f.applyPatch(items)
}
