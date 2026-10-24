// Package app is the rpak command line: plan, prefetch, run flatpak, clean up.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
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

Options:
  -h, --help     show this help
  --version      show the version

Environment:
  RPAK_FLATPAK   flatpak executable to use (default: flatpak from PATH)
  RPAK_JOBS      concurrent downloads, 1-16 (default: 4)
  RPAK_DRY_RUN   if set to 1, only show the plan and the flatpak command
`

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
	case "update", "install":
	case "help", "-h", "--help":
		fmt.Fprint(os.Stdout, usage)
		return 0
	case "--version":
		fmt.Printf("rpak %s\n", version)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "rpak: unknown command %q\n\n%s", argv[0], usage)
		return 2
	}
	command, flatpakArgs := argv[0], argv[1:]

	flatpak := os.Getenv("RPAK_FLATPAK")
	if flatpak == "" {
		flatpak = "flatpak"
		if path, err := exec.LookPath("flatpak"); err == nil {
			flatpak = path
		}
	}

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

	dirs, runFlatpak := prefetchPhase(ctx, version, command, flatpak, flatpakArgs)

	cmd := exec.Command(flatpak, append([]string{command}, flatpakArgs...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// Checking for a signal and starting flatpak under the same lock leaves
	// no window in which a signal would be neither acted on nor passed on.
	mu.Lock()
	if sig := received; sig != nil {
		mu.Unlock()
		fmt.Fprintf(os.Stderr, "rpak: interrupted, not running flatpak; partial downloads are kept for the next run\n")
		return 128 + int(sig.(syscall.Signal))
	}
	if !runFlatpak { // dry run
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
	if code == 0 && len(dirs) > 0 {
		if baseDir, err := prefetch.DefaultUserBaseDir(); err == nil {
			if err := prefetch.RemoveDirs(baseDir, dirs); err != nil {
				fmt.Fprintf(os.Stderr, "rpak: warning: cleaning up prefetched files: %v\n", err)
			}
		}
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

// prefetchPhase plans and downloads. It never fails: problems are reported
// as warnings and only reduce what is prefetched. It returns the
// directories to remove after a successful flatpak run, and false for a
// dry run, after which flatpak must not be run.
func prefetchPhase(ctx context.Context, version, command, flatpak string, flatpakArgs []string) ([]string, bool) {
	warn := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "rpak: warning: "+format+"\n", a...)
	}
	dryRun := os.Getenv("RPAK_DRY_RUN") != "" && os.Getenv("RPAK_DRY_RUN") != "0"

	baseDir, err := prefetch.DefaultUserBaseDir()
	if err != nil {
		warn("cannot find the flatpak user directory, not prefetching: %v", err)
		return nil, !dryRun
	}
	if !dryRun {
		if err := prefetch.RemoveExpired(baseDir, time.Now(), prefetch.MaxAge); err != nil {
			warn("removing expired prefetched files: %v", err)
		}
	}

	parsed := args.Parse(command, flatpakArgs)
	p := plan.Build(ctx, plan.ExecRunner{Path: flatpak, Timeout: queryTimeout}, parsed)
	if ctx.Err() != nil {
		return nil, true
	}
	for _, w := range p.Warnings {
		warn("%s", w)
	}

	if dryRun {
		if parsed.SkipReason != "" {
			fmt.Fprintf(os.Stderr, "rpak: not prefetching because of %s\n", parsed.SkipReason)
		}
		printPlan(os.Stderr, p.Sources, baseDir)
		fmt.Fprintf(os.Stderr, "rpak: dry run, would run: %s\n", shellQuote(append([]string{flatpak, command}, flatpakArgs...)))
		return nil, false
	}
	if len(p.Sources) == 0 {
		return nil, true
	}

	printPlan(os.Stderr, p.Sources, "")
	cfg := prefetch.NewConfig(baseDir, version)
	cfg.Jobs = jobs(warn)
	start := time.Now()
	summary := download(ctx, cfg, p.Sources)
	if ctx.Err() != nil {
		return nil, true
	}
	ok := summary.Downloaded + summary.Reused
	fmt.Fprintf(os.Stderr, "rpak: prefetch done: %d ready (%d downloaded, %d reused), %d skipped/failed, %s\n",
		ok, summary.Downloaded, summary.Reused, summary.Skipped+summary.Failed, time.Since(start).Round(100*time.Millisecond))
	return summary.Dirs, true
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

func printPlan(w io.Writer, sources []metadata.Source, baseDir string) {
	if len(sources) == 0 {
		if baseDir != "" {
			fmt.Fprintln(w, "rpak: nothing to prefetch")
		}
		return
	}
	var total int64
	for _, s := range sources {
		total += s.Size
	}
	fmt.Fprintf(w, "rpak: prefetching %d extra-data file(s), %s:\n", len(sources), prefetch.FormatBytes(total))
	for _, s := range sources {
		fmt.Fprintf(w, "  %s  %s  %s  %s\n", s.Ref, s.Name, prefetch.FormatBytes(s.Size), host(s.URI))
		if baseDir != "" {
			fmt.Fprintf(w, "    -> %s\n", prefetch.ExtraDataDir(baseDir, s.Checksum))
		}
	}
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
