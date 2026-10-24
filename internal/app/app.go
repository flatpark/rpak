// Package app is the rpak command line: plan, prefetch, run flatpak, clean up.
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/flatpark/rpak/internal/args"
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
  completion print the completion script for bash, zsh or fish
             (see "rpak completion --help" for how to install it)
  *          any other command runs "flatpak COMMAND ARGS..." unchanged

After a successful install or update (without --no-deploy), rpak removes
the copies of the extra-data installers that flatpak keeps in the
repositories involved.

Options:
  -h, --help     show this help
  --version      show the versions of rpak and flatpak

Environment:
  RPAK_FLATPAK   flatpak executable to use (default: flatpak from PATH)
  RPAK_JOBS      concurrent downloads, 1-16 (default: 4)
  RPAK_DRY_RUN   if set to 1, only show the plan and the flatpak command
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

// Main runs rpak with os.Args and returns the exit code.
func Main(version string) int {
	return run(version, os.Args[1:])
}

func run(version string, argv []string) int {
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
	default:
		return passthrough(argv)
	}
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

	prep := prefetchPhase(ctx, version, kind, flatpak, flatpakArgs, argv)
	defer prep.unlock()

	cmd := exec.Command(flatpak, argv...)
	// flatpak prints argv[0] in its usage ("Usage: flatpak install ...");
	// give it the name, as passthrough does, not the path.
	cmd.Args[0] = "flatpak"
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// Checking for a signal and starting flatpak under the same lock leaves
	// no window in which a signal would be neither acted on nor passed on.
	mu.Lock()
	if sig := received; sig != nil {
		mu.Unlock()
		fmt.Fprintf(os.Stderr, "rpak: interrupted, not running flatpak; partial downloads are kept for the next run\n")
		return 128 + int(sig.(syscall.Signal))
	}
	if !prep.runFlatpak { // dry run
		mu.Unlock()
		return 0
	}
	err := cmd.Start()
	if err == nil {
		child = cmd.Process
	}
	mu.Unlock()
	if err != nil {
		fmt.Fprintf(os.Stderr, "rpak: cannot run flatpak: %v\n", err)
		return 127
	}
	code := exitCode(cmd.Wait())

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
	return code
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
}

// prefetchPhase plans and runs the downloads for command ("update" or
// "install"); fullArgv is what flatpak will get, for the dry-run output.
// It never fails: problems are reported as warnings and only reduce what
// is prefetched.
func prefetchPhase(ctx context.Context, version, command, flatpak string, flatpakArgs, fullArgv []string) prepared {
	warn := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "rpak: warning: "+format+"\n", a...)
	}
	dryRun := os.Getenv("RPAK_DRY_RUN") != "" && os.Getenv("RPAK_DRY_RUN") != "0"
	parsed := args.Parse(command, flatpakArgs)
	prep := prepared{parsed: parsed, runFlatpak: !dryRun, unlock: func() {}}

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

	p := plan.Build(ctx, plan.ExecRunner{Path: flatpak, Timeout: queryTimeout}, parsed)
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

	printPlan(os.Stderr, p.Sources, "")
	cfg := prefetch.NewConfig(baseDir, version)
	cfg.Jobs = jobs(warn)
	summary := download(ctx, cfg, p.Sources)
	if ctx.Err() != nil {
		return prep
	}
	prep.dirs = summary.Dirs
	return prep
}

func jobs(warn func(string, ...any)) int {
	v := os.Getenv("RPAK_JOBS")
	if v == "" {
		return 4
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 16 {
		warn("RPAK_JOBS=%q is not a number from 1 to 16, using 4", v)
		return 4
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
