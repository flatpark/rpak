package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/flatpark/rpak/internal/release"
)

const selfUpdateUsage = `Usage: rpak self-update [VERSION] [-y] [--force] [--check]

Replaces this rpak binary with the latest release, or with release VERSION
(such as v0.3.6, also an older one), downloaded from GitHub and checked
against the release's SHA256SUMS. The binary is replaced where it is,
behind any symlink to it; rpak must be able to write to its directory (run
"sudo rpak self-update" for one in /usr/local/bin).

A development build (a version other than vMAJOR.MINOR.PATCH, such as one
built from a later commit) is only replaced with --force.

Options:
  -y, --yes      do not ask for confirmation
  --force        also replace a development build, or the same release
  --check        only show whether there is a newer release: exits with 0
                 if not, 10 if there is, 1 if it cannot tell
  -h, --help     show this help

Environment:
  RPAK_RELEASE_BASE  where the releases are, for testing and mirrors
                     (default: https://github.com/flatpark/rpak/releases)
`

// exitNewer is rpak self-update --check's exit code when there is a newer
// release, as with mise self-update --check.
const exitNewer = 10

type selfUpdateOptions struct {
	version           string // the release asked for, vMAJOR.MINOR.PATCH; "" for the latest
	yes, force, check bool
}

// parseSelfUpdateArgs reads self-update's command line. It returns a
// non-empty usage error, or help for --help.
func parseSelfUpdateArgs(argv []string) (opts selfUpdateOptions, help bool, usageErr string) {
	for _, arg := range argv {
		if arg == "-h" || arg == "--help" {
			return opts, true, ""
		}
	}
	for _, arg := range argv {
		switch arg {
		case "-y", "--yes", "--assumeyes":
			opts.yes = true
		case "--force":
			opts.force = true
		case "--check":
			opts.check = true
		default:
			if strings.HasPrefix(arg, "-") {
				return opts, false, fmt.Sprintf("unknown option %s", arg)
			}
			if opts.version != "" {
				return opts, false, "too many arguments"
			}
			v := arg
			if !strings.HasPrefix(v, "v") {
				v = "v" + v
			}
			if _, ok := release.ParseTag(v); !ok {
				return opts, false, fmt.Sprintf("%s is not a version such as v0.3.6", arg)
			}
			opts.version = v
		}
	}
	return opts, false, ""
}

// selfUpdater is one rpak self-update, with what it depends on.
type selfUpdater struct {
	version string // rpak's
	// enabled is false in a build for a package manager (main.selfUpdate).
	enabled bool
	client  *release.Client
	arch    string // runtime.GOARCH
	// executable is os.Executable: the binary to replace.
	executable func() (string, error)
	// canAsk reports whether the user can be asked (on a terminal); in
	// is where the answer comes from.
	canAsk         bool
	in             *os.File
	stdout, stderr io.Writer
}

// selfUpdateCommand is `rpak self-update`.
func selfUpdateCommand(version string, enabled bool, argv []string, stdout, stderr io.Writer) int {
	opts, help, usageErr := parseSelfUpdateArgs(argv)
	if help {
		fmt.Fprint(stdout, selfUpdateUsage)
		return 0
	}
	if usageErr != "" {
		fmt.Fprintf(stderr, "rpak self-update: %s\nSee \"rpak self-update --help\".\n", usageErr)
		return 2
	}
	u := &selfUpdater{
		version:    version,
		enabled:    enabled,
		client:     release.NewClient(version),
		arch:       runtime.GOARCH,
		executable: os.Executable,
		canAsk:     isTerminal(os.Stdin) && isTerminal(os.Stderr),
		in:         os.Stdin,
		stdout:     stdout,
		stderr:     stderr,
	}
	// A signal cancels the download, which removes the temporary file.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	return u.run(ctx, opts)
}

func (u *selfUpdater) run(ctx context.Context, o selfUpdateOptions) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(u.stderr, "rpak self-update: "+format+"\n", a...)
		return 1
	}
	if !u.enabled {
		return fail("rpak was installed by a package manager; update it with that")
	}
	current, known := release.ParseBuild(u.version)
	dev := !known || current.Dev()
	if dev && !o.force && !o.check {
		return fail("rpak %s is a development build, not a release; --force replaces it with one", u.version)
	}

	target, what := o.version, "release "+o.version
	if target == "" {
		tag, err := u.client.Latest(ctx)
		if err != nil {
			return fail("cannot find the latest release: %v", err)
		}
		target, what = tag, "the latest release "+tag
	}
	want, _ := release.ParseTag(target)
	// cmp is how target compares with this build; a build of unknown
	// version is taken as older than any release.
	cmp := 1
	if known {
		cmp = release.Compare(want, current)
	}

	if o.check {
		return u.check(o, target, what, cmp, dev, known)
	}
	if !o.force {
		switch {
		case cmp == 0 && o.version == "":
			fmt.Fprintf(u.stdout, "rpak %s is the latest release\n", u.version)
			return 0
		case cmp == 0:
			fmt.Fprintf(u.stdout, "rpak %s is installed already; --force installs it again\n", u.version)
			return 0
		case cmp < 0 && o.version == "":
			fmt.Fprintf(u.stdout, "rpak %s is newer than %s\n", u.version, what)
			return 0
		}
	}

	exe, err := u.executable()
	if err == nil {
		// A symlink in PATH stays one: what it points to is replaced.
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return fail("cannot find this rpak binary: %v", err)
	}
	dir := filepath.Dir(exe)
	if err := release.Writable(dir); err != nil {
		return fail("cannot write %s; run with sudo, or reinstall", dir)
	}

	if !o.yes {
		verb, to := "Update", "to"
		switch {
		case dev:
			verb, to = "Replace", "with"
		case cmp < 0:
			verb = "Downgrade"
		case cmp == 0:
			verb = "Reinstall"
		}
		if !u.canAsk {
			return fail("not on a terminal; add -y to %s rpak %s %s %s", strings.ToLower(verb), u.version, to, target)
		}
		fmt.Fprintf(u.stderr, "%s rpak %s %s %s? [Y/n]: ", verb, u.version, to, target)
		answer, ok := readLine(ctx, u.in)
		if !ok {
			fmt.Fprintln(u.stderr)
			return 1
		}
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "" && a != "y" && a != "yes" {
			return 1
		}
	}

	if err := u.client.Install(ctx, target, release.Asset(u.arch), exe); err != nil {
		if ctx.Err() != nil {
			return fail("interrupted; %s is unchanged", exe)
		}
		if errors.Is(err, release.ErrChecksum) {
			return fail("%v; %s is unchanged", err, exe)
		}
		return fail("%v", err)
	}
	fmt.Fprintf(u.stdout, "rpak updated: %s → %s  %s\n", u.version, target, exe)
	return 0
}

// check is self-update --check: it says how this build compares with
// target, and exits with exitNewer if target is newer.
func (u *selfUpdater) check(o selfUpdateOptions, target, what string, cmp int, dev, known bool) int {
	command := "rpak self-update"
	if o.version != "" {
		command += " " + target
	}
	switch {
	case dev:
		fmt.Fprintf(u.stdout, "rpak %s is a development build; %s\n", u.version, what)
		if known && cmp > 0 {
			return exitNewer
		}
	case cmp > 0:
		fmt.Fprintf(u.stdout, "rpak %s; %s is newer: %s\n", u.version, what, command)
		return exitNewer
	case cmp == 0 && o.version == "":
		fmt.Fprintf(u.stdout, "rpak %s is the latest release\n", u.version)
	case cmp == 0:
		fmt.Fprintf(u.stdout, "rpak %s is installed\n", u.version)
	default:
		fmt.Fprintf(u.stdout, "rpak %s is newer than %s\n", u.version, what)
	}
	return 0
}
