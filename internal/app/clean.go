package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/flatpark/rpak/internal/clean"
	"github.com/flatpark/rpak/internal/installation"
	"github.com/flatpark/rpak/internal/lock"
	"github.com/flatpark/rpak/internal/prefetch"
)

const cleanUsage = `Usage: rpak clean [--user | --system | --installation=NAME]... [--dry-run] [-v]

Frees the space stock flatpak leaves behind:

  - the copy of every extra-data installer that flatpak keeps in the
    detached metadata of the commit it came with (xa.extra-data), in
    every commit of the repository; signatures and everything else in
    there are kept as they are
  - rpak's prefetch directories (those with a .rpak-prefetch marker) in
    the user's extra-data directory
  - empty directories under repo/refs, partial-commit markers of commits
    that are gone, checkouts of interrupted deploys, empty app/runtime
    directories of interrupted installs, and old deployments in .removed
    that no running app uses

By default the default system installation, the user installation and
every other configured installation are looked at. System installations
that are not writable are only inspected; run "sudo rpak clean --system"
for those. Repositories are locked exclusively while they are cleaned; if
flatpak is busy with one, it is skipped.

Options:
  -u, --user               the user installation
  --system                 the default system installation
  --installation=NAME      the system installation NAME
  --dry-run                only show what would be freed; changes nothing
  -v, --verbose            also show what is kept, and why
  -h, --help               show this help
`

type cleanOptions struct {
	installations []string
	dryRun        bool
	verbose       bool
}

// parseCleanArgs reads clean's command line. It returns a non-empty usage
// error, or help for --help.
func parseCleanArgs(argv []string) (opts cleanOptions, help bool, usageErr string) {
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case arg == "--user":
			opts.installations = append(opts.installations, "--user")
		case arg == "--system":
			opts.installations = append(opts.installations, "--system")
		case arg == "--installation" && i+1 < len(argv):
			i++
			opts.installations = append(opts.installations, "--installation="+argv[i])
		case strings.HasPrefix(arg, "--installation="):
			opts.installations = append(opts.installations, arg)
		case arg == "--dry-run":
			opts.dryRun = true
		case arg == "--verbose":
			opts.verbose = true
		case arg == "--ostree-verbose":
		case arg == "--help":
			return opts, true, ""
		case strings.HasPrefix(arg, "--"):
			return opts, false, fmt.Sprintf("unknown option %s", arg)
		case strings.HasPrefix(arg, "-") && len(arg) > 1:
			for _, c := range arg[1:] {
				switch c {
				case 'u':
					opts.installations = append(opts.installations, "--user")
				case 'v':
					opts.verbose = true
				case 'h', '?':
					return opts, true, ""
				default:
					return opts, false, fmt.Sprintf("unknown option -%c", c)
				}
			}
		default:
			return opts, false, "too many arguments"
		}
	}
	return opts, false, ""
}

// underSudo is running_under_sudo_root(), for rpak: root, run by sudo as
// rpak (or flatpak).
func underSudo() bool {
	command, ok := os.LookupEnv("SUDO_COMMAND")
	if !ok || os.Geteuid() != 0 {
		return false
	}
	first, _, _ := strings.Cut(command, " ")
	return strings.HasSuffix(first, "rpak") || strings.HasSuffix(first, "flatpak")
}

// mb formats a size as flatpak clean's per-item lines do: MiB, called MB.
func mb(size int64) string {
	return fmt.Sprintf("%.1f MB", float64(size)/(1024*1024))
}

// formatSize is g_format_size(): decimal units.
func formatSize(size int64) string {
	if size < 1000 {
		if size == 1 {
			return "1 byte"
		}
		return fmt.Sprintf("%d bytes", size)
	}
	value := float64(size)
	for _, unit := range []string{"kB", "MB", "GB", "TB", "PB"} {
		value /= 1000
		if value < 1000 {
			return fmt.Sprintf("%.1f %s", value, unit)
		}
	}
	return fmt.Sprintf("%.1f EB", value/1000)
}

// sudoHint is the command that cleans a system installation as root.
func sudoHint(inst installation.Installation) string {
	return fmt.Sprintf(`sudo rpak clean %s`, inst.Option())
}

// cleanCommand is `rpak clean`; the output follows the fork's flatpak clean:
// a heading per installation if there are several, a line per item, a line
// for installations that could only be inspected, and a total.
func cleanCommand(argv []string, stdout, stderr io.Writer) int {
	opts, help, usageErr := parseCleanArgs(argv)
	if help {
		fmt.Fprint(stdout, cleanUsage)
		return 0
	}
	if usageErr != "" {
		fmt.Fprintf(stderr, "rpak clean: %s\nSee \"rpak clean --help\".\n", usageErr)
		return 2
	}
	warn := func(format string, a ...any) { fmt.Fprintf(stderr, "rpak: warning: "+format+"\n", a...) }
	errorf := func(format string, a ...any) { fmt.Fprintf(stderr, "rpak: error: "+format+"\n", a...) }
	var verbose func(string, ...any)
	if opts.verbose {
		verbose = func(format string, a ...any) { fmt.Fprintf(stderr, "rpak: "+format+"\n", a...) }
	}
	for _, o := range opts.installations {
		if o == "--user" && underSudo() {
			errorf("refusing to operate under sudo with --user; omit sudo to clean the user installation")
			return 1
		}
	}

	insts, warnings, err := installation.Env{}.Select(opts.installations, true)
	if err != nil {
		errorf("%v", err)
		return 1
	}
	for _, w := range warnings {
		warn("%s", w)
	}
	explicit := len(opts.installations) > 0
	showTitles := len(insts) > 1
	item := func(size int64, what string) { fmt.Fprintf(stdout, "%s  %s\n", mb(size), what) }

	code := 0
	var objects int
	var freed int64
	for _, inst := range insts {
		haveRepo := clean.HasRepo(inst.Path)
		haveExtraData := false
		if inst.User {
			_, err := os.Stat(filepath.Join(inst.Path, "extra-data"))
			haveExtraData = err == nil
		}
		if !haveRepo && !inst.User {
			if verbose != nil {
				verbose("%s: no repository at %s", inst.Title(), filepath.Join(inst.Path, "repo"))
			}
			continue
		}
		if showTitles && (haveRepo || haveExtraData) {
			fmt.Fprintf(stdout, "%s:\n", inst.Title())
		}

		if haveRepo {
			c := &clean.Cleaner{DryRun: opts.dryRun, Item: item, Verbose: verbose, Warn: warn}
			readOnly, err := cleanRepo(c, inst, explicit)
			if err != nil {
				errorf("%s: %v", inst.Title(), err)
				code = 1
			} else {
				if readOnly {
					if inst.User {
						fmt.Fprintf(stdout, "%s: would free %s; the repository is not writable\n", inst.Title(), formatSize(c.Freed))
					} else {
						fmt.Fprintf(stdout, "%s: would free %s; run \"%s\"\n", inst.Title(), formatSize(c.Freed), sudoHint(inst))
					}
				}
				if opts.dryRun || !readOnly {
					objects += c.Objects
					freed += c.Freed
				}
				if c.Failed > 0 {
					code = 1
				}
			}
		}

		if inst.User && haveExtraData {
			c := &clean.Cleaner{DryRun: opts.dryRun, Item: item, Verbose: verbose, Warn: warn}
			release := func() {}
			if !opts.dryRun {
				release, err = prefetch.TryLockExclusive(inst.Path)
				if err != nil {
					if errors.Is(err, prefetch.ErrInUse) {
						warn("%s: %v, not cleaning them", filepath.Join(inst.Path, "extra-data"), err)
					} else {
						errorf("%v", err)
						code = 1
					}
					continue
				}
			}
			c.PrefetchDirs(inst.Path)
			release()
			objects += c.Objects
			freed += c.Freed
			if c.Failed > 0 {
				code = 1
			}
		}
	}

	verb := "Freed"
	if opts.dryRun {
		verb = "Would free"
	}
	fmt.Fprintf(stdout, "%s: %d objects, %.2f GB\n", verb, objects, float64(freed)/(1024*1024*1024))
	return code
}

// cleanRepo is clean_repo(): checks that the repository may be changed,
// locks it, and runs a. and c. on it. A repository that is not writable is
// only inspected, as a dry run, unless the installation was asked for
// explicitly, which is an error.
func cleanRepo(c *clean.Cleaner, inst installation.Installation, explicit bool) (readOnly bool, err error) {
	repo := filepath.Join(inst.Path, "repo")
	if !clean.Writable(inst.Path) {
		if explicit {
			return false, fmt.Errorf("the repository at %s is not writable; run this command as root", repo)
		}
		readOnly = true
		c.DryRun = true
	}
	// A dry run changes nothing, not even lock files, so it takes no locks.
	if !c.DryRun {
		release, err := clean.Lock(inst.Path)
		if err != nil {
			return false, err
		}
		defer release()
	}
	c.StripExtraData(repo)
	c.RepoLeftovers(repo)
	c.InterruptedDeploys(inst.Path)
	c.Removed(inst.Path)
	return readOnly, nil
}

// reclaim strips xa.extra-data from the repositories of the installations
// an install or update used, after flatpak succeeded. It is quiet apart
// from a total, never waits for a lock and never fails: problems are
// warnings.
func reclaim(options []string, stderr io.Writer) {
	warn := func(format string, a ...any) { fmt.Fprintf(stderr, "rpak: warning: "+format+"\n", a...) }
	insts, _, err := installation.Env{}.Select(options, false)
	if err != nil {
		warn("not freeing cached extra-data: %v", err)
		return
	}
	var freed int64
	for _, inst := range insts {
		if !clean.HasRepo(inst.Path) {
			continue
		}
		if !clean.Writable(inst.Path) {
			if inst.User {
				continue
			}
			c := &clean.Cleaner{DryRun: true}
			c.StripExtraData(filepath.Join(inst.Path, "repo"))
			if c.Freed > 0 {
				fmt.Fprintf(stderr, "rpak: run \"%s\" to free %s of cached extra-data\n", sudoHint(inst), mb(c.Freed))
			}
			continue
		}
		release, err := clean.Lock(inst.Path)
		if err != nil {
			if errors.Is(err, lock.ErrBusy) {
				warn("%s: the repository is busy, not freeing cached extra-data; run \"rpak clean\" later", inst.Title())
			} else {
				warn("%s: not freeing cached extra-data: %v", inst.Title(), err)
			}
			continue
		}
		c := &clean.Cleaner{Warn: warn}
		c.StripExtraData(filepath.Join(inst.Path, "repo"))
		release()
		freed += c.Freed
	}
	if freed > 0 {
		fmt.Fprintf(stderr, "rpak: freed %s of cached extra-data\n", mb(freed))
	}
}
