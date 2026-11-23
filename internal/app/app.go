// Package app is the rpak command line: plan, prefetch, run flatpak, clean up.
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/flatpark/rpak/internal/args"
	"github.com/flatpark/rpak/internal/metadata"
	"github.com/flatpark/rpak/internal/plan"
	"github.com/flatpark/rpak/internal/prefetch"
)

const usage = `Usage: rpak COMMAND [flatpak options and arguments...]

Downloads the extra-data of the apps flatpak is about to install or update,
several at a time, then runs flatpak with the same arguments.

Commands:
  update     prefetch, then run "flatpak update ARGS..."
  install    prefetch, then run "flatpak install ARGS..."
  clean      free the space flatpak leaves behind (see "rpak clean --help")
  doctor     show what to paste into a problem report, and what looks wrong
             (see "rpak doctor --help")
  completion print the completion script for bash, zsh or fish
             (see "rpak completion --help" for how to install it)
  patch      list, make or undo changes to installed apps (experimental,
             see below and "rpak patch --help")
  self-update
             replace rpak with its latest release
             (see "rpak self-update --help")
  *          any other command runs "flatpak COMMAND ARGS..." unchanged

On a terminal and without -y, flatpak starts at once and asks while rpak
prefetches; a "yes" reaches it when the prefetch is done.

After a successful install or update (without --no-deploy), rpak removes
the copies of the extra-data installers that flatpak keeps in the
repositories involved.

Experimental: install, update and patch take --force-runtime[=TARGET],
which makes the apps run on a newer branch of their runtime than they were
built for (TARGET: latest, the default, a branch such as 26.08, or a
runtime ID/ARCH/BRANCH); the user installation only, install takes the
apps (and their REMOTE, unless only one remote has them), update and patch
the apps. Later installs and updates keep it. The
apps were not tested on that runtime and may break; "rpak patch --reset
APP" undoes it, "rpak patch" lists what is patched.

Experimental too: --extra-data=[NAME=]URL (install, update and patch, one
app at a time) downloads the app's extra-data file NAME from URL instead,
such as a vendor's newer release or a link that still works, until
upstream changes that file itself. The file is your choice; "rpak patch
--reset=extra-data APP" undoes it.

Options:
  -h, --help     show this help
  --version      show the versions of rpak and flatpak

Environment:
  RPAK_FLATPAK   flatpak executable to use (default: flatpak from PATH)
  RPAK_JOBS      concurrent downloads, 1-16 (default: 4)
  RPAK_DRY_RUN   if set to 1, only show the plan and the flatpak command
  RPAK_RELEASE_BASE
                 where rpak self-update and doctor look for rpak's
                 releases, for testing and mirrors (default:
                 https://github.com/flatpark/rpak/releases)
`

// flatpakPath is the flatpak to run: $RPAK_FLATPAK, or flatpak from PATH.
func flatpakPath() string {
	flatpak := os.Getenv("RPAK_FLATPAK")
	if flatpak == "" {
		flatpak = "flatpak"
	}
	if path, err := exec.LookPath(flatpak); err == nil {
		return path
	}
	return flatpak
}

// leadingGlobal reports whether arg is a global flatpak option that may come
// before the command and is not one that makes flatpak exit right away
// (--version, --installations, ...): -v, -vv, --verbose, --ostree-verbose.
func leadingGlobal(arg string) bool {
	if arg == "--verbose" || arg == "--ostree-verbose" {
		return true
	}
	return len(arg) > 1 && strings.Trim(arg[1:], "v") == "" && arg[0] == '-'
}

// passthrough replaces rpak with flatpak for every command rpak does not
// handle itself, so that it behaves exactly like flatpak: same process,
// signals, terminal and exit code.
func passthrough(argv []string) int {
	flatpak := flatpakPath()
	if os.Getenv("RPAK_DRY_RUN") == "1" {
		fmt.Fprintf(os.Stderr, "rpak: dry run, would run: %s\n", shellQuote(append([]string{flatpak}, argv...)))
		return 0
	}
	err := syscall.Exec(flatpak, append([]string{"flatpak"}, argv...), os.Environ())
	fmt.Fprintf(os.Stderr, "rpak: cannot run flatpak: %v\n", err)
	return 127
}

// queryTimeout bounds each flatpak query command run while planning.
const queryTimeout = 2 * time.Minute

// Main runs rpak with os.Args and returns the exit code. selfUpdate is
// main.selfUpdate: "disabled" in a build that a package manager installs,
// which then updates rpak, not rpak self-update.
func Main(version, selfUpdate string) int {
	return run(version, selfUpdate != "disabled", os.Args[1:])
}

func run(version string, selfUpdate bool, argv []string) int {
	if len(argv) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	switch argv[0] {
	case "help", "-h", "--help":
		fmt.Fprint(os.Stdout, usage)
		return 0
	case "--version":
		fmt.Printf("rpak %s\n", version)
		return passthrough(argv)
	case "complete": // the shell completion protocol, as flatpak's main() takes it
		if len(argv) >= 3 {
			return completeCommand(argv[1:], os.Stdout)
		}
	}
	// flatpak takes global options before the command ("flatpak -v install").
	i := 0
	for i < len(argv) && leadingGlobal(argv[i]) {
		i++
	}
	if i == len(argv) {
		return passthrough(argv)
	}
	command, kind := argv[i], argv[i]
	switch command {
	case "upgrade": // flatpak's hidden alias of update
		kind = "update"
	case "update", "install":
	case "clean":
		return cleanCommand(append(argv[:i:i], argv[i+1:]...), os.Stdout, os.Stderr)
	case "completion":
		return completionCommand(argv[i+1:], os.Stdout, os.Stderr)
	case "doctor":
		return doctorCommand(version, selfUpdate, argv[i+1:], os.Stdout, os.Stderr)
	case "self-update":
		return selfUpdateCommand(version, selfUpdate, argv[i+1:], os.Stdout, os.Stderr)
	case "patch":
		return patchCommand(version, argv[:i], argv[i+1:], os.Stdout, os.Stderr)
	default:
		return passthrough(argv)
	}
	return transaction(version, kind, argv, i)
}

// runFlatpak runs one flatpak update/install: argv is flatpak's whole
// command line, argv[i] the command, kind "update" or "install". It
// prefetches, runs flatpak, removes what it prefetched and frees the
// extra-data flatpak cached, and returns flatpak's exit code.
func runFlatpak(version, kind string, argv []string, i int) int {
	return runFlatpakWith(version, kind, argv, i, false).code
}

// flatpakRun is the outcome of runFlatpakWith: the exit code, the apps to
// patch, and whether flatpak was stopped at its question (see
// runInteractive) for a file replaced upstream.
type flatpakRun struct {
	code    int
	patch   []changeJob
	stopped bool
}

// runFlatpakWith is runFlatpak; with offer, it offers to patch apps whose
// files were replaced upstream (see changed.go) and returns those to
// patch: flatpak then did not run, or stopped, or failed.
func runFlatpakWith(version, kind string, argv []string, i int, offer bool) flatpakRun {
	flatpakArgs := argv[i+1:]
	flatpak := flatpakPath()

	// Signals are caught from the start: during prefetching they stop the
	// downloads and rpak exits without running flatpak; once flatpak runs
	// they are passed on to it.
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var (
		mu       sync.Mutex
		child    *os.Process
		received os.Signal
	)
	go func() {
		for sig := range signals {
			mu.Lock()
			if child != nil {
				_ = child.Signal(sig)
			} else if received == nil {
				received = sig
				cancel()
			}
			mu.Unlock()
		}
	}()

	prep := prefetchPhase(ctx, version, kind, flatpak, flatpakArgs, argv, offer)
	defer prep.unlock()
	if len(prep.patch) > 0 {
		// Asked before flatpak: the patch runs instead. What was
		// prefetched stays for it.
		return flatpakRun{patch: prep.patch}
	}

	cmd := exec.Command(flatpak, argv...)
	// flatpak prints argv[0] in its usage ("Usage: flatpak install ...");
	// give it the name, as passthrough does, not the path.
	cmd.Args[0] = "flatpak"
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// start starts flatpak, unless a signal came first or this is a dry
	// run; if it does not, code is rpak's exit code. Checking for a signal
	// and starting flatpak under the same lock leaves no window in which a
	// signal would be neither acted on nor passed on.
	start := func() (code int, started bool) {
		mu.Lock()
		defer mu.Unlock()
		if sig := received; sig != nil {
			fmt.Fprintf(os.Stderr, "rpak: interrupted, not running flatpak; partial downloads are kept for the next run\n")
			return 128 + int(sig.(syscall.Signal)), false
		}
		if !prep.runFlatpak { // dry run
			return 0, false
		}
		if err := cmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "rpak: cannot run flatpak: %v\n", err)
			return 127, false
		}
		child = cmd.Process
		return 0, true
	}
	var code int
	if prep.console != nil {
		var started bool
		if code, started = prep.runInteractive(ctx, cmd, start); !started {
			return flatpakRun{code: code}
		}
	} else {
		var started bool
		if code, started = start(); !started {
			return flatpakRun{code: code}
		}
		code = exitCode(cmd.Wait())
	}
	mu.Lock()
	child = nil
	mu.Unlock()
	// Found while flatpak was asking (interactive mode): flatpak has
	// downloaded the file itself and failed on it.
	var patch []changeJob
	if prep.stopped && ctx.Err() == nil {
		fmt.Fprintf(os.Stderr, "\nrpak: Stopped flatpak at its question: it would download a file that was replaced upstream, and fail.\n")
	}
	if code != 0 && len(prep.changed) > 0 && ctx.Err() == nil {
		patch = offerChanges(ctx, os.Stderr, os.Stdin, kind, prep.changed, prep.origins, prep.offer)
	}

	// Remove what we placed only when flatpak succeeded; after a failure or
	// a "no" at its prompt the files are kept for the next attempt (and
	// expire after prefetch.MaxAge).
	if code == 0 && len(prep.dirs) > 0 {
		if baseDir, err := prefetch.DefaultUserBaseDir(); err == nil {
			if err := prefetch.RemoveDirs(baseDir, prep.dirs); err != nil {
				fmt.Fprintf(os.Stderr, "rpak: warning: cleaning up prefetched files: %v\n", err)
			}
		}
	}
	prep.unlock()
	// flatpak has just written the installers into the commits it pulled.
	// After --no-deploy they are what a later --no-pull deploy installs.
	if code == 0 && prep.parsed.SkipReason != "--help" && !prep.parsed.NoDeploy {
		reclaim(prep.parsed.Installations, os.Stderr)
	}
	return flatpakRun{code: code, patch: patch, stopped: prep.stopped}
}

// exitCode turns flatpak's exit status into rpak's: the same code, or
// 128+signal if it was killed by a signal.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal())
		}
		return exit.ExitCode()
	}
	fmt.Fprintf(os.Stderr, "rpak: waiting for flatpak: %v\n", err)
	return 1
}

// prepared is the outcome of prefetchPhase.
type prepared struct {
	parsed args.Parsed
	// dirs are the directories to remove after a successful flatpak run.
	dirs []string
	// runFlatpak is false for a dry run, after which flatpak must not run.
	runFlatpak bool
	// unlock releases the prefetch directories (see prefetch.LockShared)
	// once they have been removed; it may be called more than once.
	unlock func()

	// For the interactive mode (interactive.go): the pseudo-terminal for
	// flatpak's stdin, and the downloads still to do, under the lock.
	console *console
	sources []metadata.Source
	cfg     prefetch.Config

	// origins are the plan's; changed are the files found replaced
	// upstream in interactive mode, to tell about after flatpak; patch
	// are the apps to patch instead of running flatpak.
	origins map[[2]string][]plan.Origin
	changed []prefetch.Change
	patch   []changeJob
	// offer is runFlatpakWith's; stopped reports that flatpak was stopped
	// at its question for a changed file.
	offer, stopped bool
}

// runInteractive starts flatpak with its stdin on the pseudo-terminal and
// downloads in the background, then waits for flatpak. When flatpak exits
// the downloads still running are canceled, and the summary line follows
// flatpak's output if they all ended. prep.dirs is set only then: the
// directories of a canceled run are kept for the next one.
func (prep *prepared) runInteractive(ctx context.Context, cmd *exec.Cmd, start func() (int, bool)) (code int, started bool) {
	c := prep.console
	defer c.close()
	cmd.Stdin = c.slave
	if code, started := start(); !started {
		return code, false
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	t := detectTerm(os.Stderr)
	rep := newReporter(os.Stderr, t, prep.sources)
	rep.transient, rep.quiet = true, true
	var (
		summary  prefetch.Summary
		complete bool
		done     = make(chan struct{})
	)
	go func() {
		defer close(done)
		summary = rep.run(ctx, prep.cfg, prep.sources)
		complete = ctx.Err() == nil
	}()
	// A file replaced upstream: flatpak would download it and fail; it is
	// stopped at its question instead, and rpak asks its own (see
	// runFlatpakWith).
	var veto func() bool
	if prep.offer {
		veto = func() bool {
			select {
			case <-done:
				if complete && len(summary.Changed) > 0 {
					prep.stopped = true
					return true
				}
			default:
			}
			return false
		}
	}
	stop := c.start(func() <-chan struct{} {
		if rep.show() {
			return done
		}
		return nil
	}, veto)
	code = exitCode(cmd.Wait())
	stop()
	cancel()
	<-done
	if complete {
		prep.dirs = summary.Dirs
		prep.changed = summary.Changed
		writeLines(os.Stderr, t, []line{rep.summary(summary)})
	}
	return code, true
}

// prefetchPhase plans and runs the downloads for command ("update" or
// "install"); fullArgv is what flatpak will get, for the dry-run output.
// It never fails: problems are reported as warnings and only reduce what
// is prefetched.
func prefetchPhase(ctx context.Context, version, command, flatpak string, flatpakArgs, fullArgv []string, offer bool) prepared {
	warn := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "rpak: warning: "+format+"\n", a...)
	}
	dryRun := os.Getenv("RPAK_DRY_RUN") != "" && os.Getenv("RPAK_DRY_RUN") != "0"
	parsed := args.Parse(command, flatpakArgs)
	prep := prepared{parsed: parsed, runFlatpak: !dryRun, unlock: func() {}, offer: offer && canAsk(parsed)}

	baseDir, err := prefetch.DefaultUserBaseDir()
	if err != nil {
		warn("cannot find the flatpak user directory, not prefetching: %v", err)
		return prep
	}
	locked := false
	lockDirs := func(create bool) bool {
		release, ok, err := prefetch.LockShared(baseDir, create)
		if err != nil {
			warn("not prefetching: %v", err)
			return false
		}
		var once sync.Once
		prep.unlock = func() { once.Do(release) }
		locked = ok
		return true
	}
	if !dryRun {
		if !lockDirs(false) {
			return prep
		}
		if err := prefetch.RemoveExpired(baseDir, time.Now(), prefetch.MaxAge); err != nil {
			warn("removing expired prefetched files: %v", err)
		}
	}

	status := newPlanStatus(os.Stderr, detectTerm(os.Stderr))
	p := plan.Build(ctx, plan.ExecRunner{Path: flatpak, Timeout: queryTimeout}, parsed, status.watch)
	status.close()
	if ctx.Err() != nil {
		return prep
	}
	for _, w := range p.Warnings {
		warn("%s", w)
	}

	if dryRun {
		if parsed.SkipReason != "" {
			fmt.Fprintf(os.Stderr, "rpak: not prefetching because of %s\n", parsed.SkipReason)
		}
		printPlan(os.Stderr, p.Sources, baseDir)
		fmt.Fprintf(os.Stderr, "rpak: dry run, would run: %s\n", shellQuote(append([]string{flatpak}, fullArgv...)))
		return prep
	}
	if len(p.Sources) == 0 {
		return prep
	}
	if !locked {
		prep.unlock()
		if !lockDirs(true) {
			return prep
		}
	}

	cfg := prefetch.NewConfig(baseDir, version)
	cfg.Jobs = jobs(warn)
	prep.origins = p.Origins
	sources := withoutChanged(p.Sources, nil)
	ask := offer && canAsk(parsed)
	if offer {
		// Before flatpak shows anything: the servers that tell the size or
		// the checksum of their files show a replaced one at once.
		changes := prefetch.New(cfg).Probe(ctx, sources)
		if ctx.Err() != nil {
			return prep
		}
		if len(changes) > 0 {
			if prep.patch = offerChanges(ctx, os.Stderr, os.Stdin, command, changes, p.Origins, ask); len(prep.patch) > 0 || ctx.Err() != nil {
				return prep
			}
			sources = withoutChanged(sources, changes)
		}
	}
	if len(sources) == 0 {
		return prep
	}
	if interactive(parsed) == "" {
		c, err := openConsole()
		if err == nil {
			prep.console, prep.sources, prep.cfg = c, sources, cfg
			return prep
		}
		warnSequential(err)
	}
	printPlan(os.Stderr, sources, "")
	summary := download(ctx, cfg, sources)
	if ctx.Err() != nil {
		return prep
	}
	prep.dirs = summary.Dirs
	if len(summary.Changed) > 0 {
		prep.patch = offerChanges(ctx, os.Stderr, os.Stdin, command, summary.Changed, p.Origins, ask)
	}
	return prep
}

func jobs(warn func(string, ...any)) int {
	v := os.Getenv("RPAK_JOBS")
	n, ok := prefetch.ParseJobs(v)
	if !ok {
		warn("RPAK_JOBS=%q is not a number from 1 to %d, using %d", v, prefetch.MaxJobs, n)
	}
	return n
}

// shellQuote formats argv so that it can be pasted into a shell.
func shellQuote(argv []string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		if a != "" && strings.Trim(a, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./=:,+@%") == "" {
			quoted[i] = a
		} else {
			quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
	}
	return strings.Join(quoted, " ")
}
