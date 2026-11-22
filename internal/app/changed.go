package app

// Files replaced upstream under the same URL.
//
// A vendor that rebuilds a release without changing its download link
// breaks every app pinned to it: flatpak checks the size and the checksum
// of what it downloads against the commit and fails. rpak finds out
// before flatpak does: it asks the servers (prefetch.Downloader.Probe)
// before downloading, and checks every file it downloads anyway. For such
// a file it offers to install the app with the file as it is now, from the
// same URL: an extra-data patch (rpak install/update --extra-data), which
// lasts until upstream repins the app. A file rpak has downloaded already
// is used for the patch as it is.
//
// Only the top-level install or update offers (offer in runFlatpakWith):
// the runs of a patch flow hold its lock and only say what to run.

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/flatpark/rpak/internal/args"
	"github.com/flatpark/rpak/internal/extradata"
	fr "github.com/flatpark/rpak/internal/forceruntime"
	"github.com/flatpark/rpak/internal/installation"
	"github.com/flatpark/rpak/internal/metadata"
	"github.com/flatpark/rpak/internal/plan"
	"github.com/flatpark/rpak/internal/prefetch"
)

// changeJob is a changed source to patch, in the app of origin.
type changeJob struct {
	change prefetch.Change
	origin plan.Origin
}

var (
	// declined are the changed sources (plan.OriginKey of the source) the
	// user said no to; they are not asked about again by this process.
	declined = map[[2]string]bool{}
	// knownFiles are downloaded changed files, by URL and name, which a
	// patch uses instead of downloading them again (replacements).
	knownFiles = map[[2]string]prefetch.Change{}
)

// withoutChanged returns sources without those of changes and those
// declined before: flatpak downloads them itself, and fails.
func withoutChanged(sources []metadata.Source, changes []prefetch.Change) []metadata.Source {
	skip := map[[2]string]bool{}
	for _, c := range changes {
		skip[plan.OriginKey(c.Source)] = true
	}
	var out []metadata.Source
	for _, s := range sources {
		if !skip[plan.OriginKey(s)] && !declined[plan.OriginKey(s)] {
			out = append(out, s)
		}
	}
	return out
}

// canAsk reports whether rpak may ask the user a question: on a terminal,
// without -y.
func canAsk(parsed args.Parsed) bool {
	return !parsed.AssumeYes && isTerminal(os.Stdin) && isTerminal(os.Stderr)
}

// offerChanges tells about changed sources and, if ask, asks for each
// whether to patch its app; it returns those to patch. Without ask it says
// what to run instead. A source of no app of the user installation cannot
// be patched.
func offerChanges(ctx context.Context, w io.Writer, in *os.File, kind string, changes []prefetch.Change, origins map[[2]string][]plan.Origin, ask bool) []changeJob {
	var jobs []changeJob
	for _, c := range changes {
		key := plan.OriginKey(c.Source)
		if declined[key] {
			continue
		}
		var user *plan.Origin
		for _, o := range origins[key] {
			if o.Installation == args.User && strings.HasPrefix(o.Ref, "app/") {
				user = &o
				break
			}
		}
		describeChange(w, c, origins[key])
		if user == nil {
			fmt.Fprintf(w, "rpak: Only apps of the user installation can be patched to use it; flatpak will fail on it until the app is updated.\n")
			declined[key] = true
			continue
		}
		cmd := changeCommand(kind, c, *user)
		if !ask {
			fmt.Fprintf(w, "rpak: To install it with the file as it is now (experimental): %s\n", shellQuote(cmd))
			declined[key] = true
			continue
		}
		fmt.Fprintf(w, "Install %s with the file as it is now, as an experimental extra-data patch (its packagers did not check it)? [y/N]: ", idOf(user.Ref))
		answer, ok := readLine(ctx, in)
		if !ok {
			fmt.Fprintln(w)
			return nil
		}
		if !answersYes(answer) {
			declined[key] = true
			fmt.Fprintf(w, "rpak: Not patched; flatpak will fail on it. Later: %s\n", shellQuote(cmd))
			continue
		}
		jobs = append(jobs, changeJob{change: c, origin: *user})
	}
	return jobs
}

// describeChange says which file changed, and how.
func describeChange(w io.Writer, c prefetch.Change, origins []plan.Origin) {
	var ids []string
	seen := map[string]bool{}
	for _, o := range origins {
		if id := idOf(o.Ref); !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		ids = []string{idOf(c.Source.Ref)}
	}
	now := humanSize(c.Got.Size)
	if c.Got.Size <= 0 {
		now = "size unknown"
	}
	if c.Got.Checksum != "" {
		now += ", sha256 " + c.Got.Checksum
	} else {
		now += ", sha256 not known until downloaded"
	}
	fmt.Fprintf(w, "rpak: %s: %s is not the file the app was built with; upstream has likely replaced it at the same URL.\n", strings.Join(ids, ", "), sanitize(c.Source.Name))
	fmt.Fprintf(w, "rpak:   URL:        %s\n", sanitize(c.Source.URI))
	fmt.Fprintf(w, "rpak:   built with: %s, sha256 %s\n", humanSize(c.Source.Size), c.Source.Checksum)
	fmt.Fprintf(w, "rpak:   served now: %s\n", now)
}

// changeCommand is the rpak command that patches the app of origin to the
// file as it is now (without the global options).
func changeCommand(kind string, c prefetch.Change, o plan.Origin) []string {
	argv := []string{"rpak", kind, args.User, extraDataArg(c)}
	if kind == "install" {
		argv = append(argv, o.Remote)
	}
	return append(argv, idOf(o.Ref))
}

func extraDataArg(c prefetch.Change) string {
	return fmt.Sprintf("%s=%s=%s", extradata.Option, c.Source.Name, c.Source.URI)
}

// readLine reads one line of the terminal, or nothing if ctx ends first
// (a signal) or the input does. A terminal in canonical mode returns at
// most a line per read, so nothing meant for flatpak is taken.
func readLine(ctx context.Context, in *os.File) (string, bool) {
	type result struct {
		s  string
		ok bool
	}
	ch := make(chan result, 1)
	go func() {
		buf := make([]byte, 1024)
		n, err := in.Read(buf)
		ch <- result{string(buf[:n]), err == nil || n > 0}
	}()
	select {
	case r := <-ch:
		return r.s, r.ok
	case <-ctx.Done():
		return "", false
	}
}

func answersYes(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "y" || s == "yes"
}

// patchChanges patches the apps of jobs, then runs what is left of the
// command (argv, the command at i): the other refs of an install, or the
// update again. It returns the first failure's exit code, or 0.
func patchChanges(version, kind string, argv []string, i int, parsed args.Parsed, jobs []changeJob) int {
	globals := argv[:i]
	opts := withoutInstallations(kind, args.Options(kind, argv[i+1:]))
	code := 0
	var patched []string
	for _, j := range jobs {
		if j.change.Dir != "" {
			knownFiles[[2]string{j.change.Source.URI, j.change.Source.Name}] = j.change
		}
		// The user has just said yes to this, which is all flatpak asks.
		cmd := append(append([]string(nil), globals...), kind, args.User, "-y")
		cmd = append(cmd, opts...)
		cmd = append(cmd, extraDataArg(j.change))
		if kind == "install" {
			cmd = append(cmd, j.origin.Remote)
		}
		cmd = append(cmd, j.origin.Ref)
		if c := transaction(version, kind, cmd, len(globals)); c != 0 && code == 0 {
			code = c
		}
		patched = append(patched, j.origin.Ref)
	}
	matched := func(arg string) bool {
		for _, ref := range patched {
			if plan.Matches(ref, arg) {
				return true
			}
		}
		return false
	}
	var rest []string
	switch kind {
	case "install":
		// The remote the plan found the apps in is one, whatever the
		// installations' configuration says.
		remote, refs := args.SplitInstallRemote(parsed.Positionals, func(name string) bool {
			for _, j := range jobs {
				if j.origin.Remote == name {
					return true
				}
			}
			return isAnyRemote(name)
		})
		var left []string
		for _, r := range refs {
			if !matched(r) {
				left = append(left, r)
			}
		}
		if len(left) == 0 {
			return code
		}
		rest = append(append([]string(nil), globals...), kind)
		rest = append(rest, args.Options(kind, argv[i+1:])...)
		if remote != "" {
			rest = append(rest, remote)
		}
		rest = append(rest, left...)
	default:
		all := len(parsed.Positionals) > 0
		for _, p := range parsed.Positionals {
			all = all && matched(p)
		}
		if all {
			return code
		}
		rest = argv
	}
	if c := transaction(version, kind, rest, i); c != 0 && code == 0 {
		code = c
	}
	return code
}

// withoutInstallations drops the installation options from opts: a patch
// is for the user installation.
func withoutInstallations(kind string, opts []string) []string {
	var out []string
	for k := 0; k < len(opts); k++ {
		o := opts[k]
		name, _, hasValue := strings.Cut(o, "=")
		switch {
		case o == args.User || o == args.System:
		case name == "--installation":
			if !hasValue && args.TakesValue(kind, name) {
				k++
			}
		default:
			out = append(out, o)
		}
	}
	return out
}

// isAnyRemote reports whether name is a remote of an installation.
func isAnyRemote(name string) bool {
	for _, inst := range (installation.Env{}).All() {
		remotes, _ := fr.Remotes(inst.Path)
		for _, r := range remotes {
			if r.Name == name {
				return true
			}
		}
	}
	return false
}
