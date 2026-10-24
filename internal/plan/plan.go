// Package plan asks flatpak which refs an update/install command is going to
// pull and collects their extra-data sources.
//
// Everything here is best effort: a query that fails only means rpak
// prefetches less, and flatpak downloads the rest itself.
package plan

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/flatpark/rpak/internal/args"
	"github.com/flatpark/rpak/internal/metadata"
)

// Runner runs a flatpak query command and returns its standard output.
type Runner interface {
	Output(ctx context.Context, argv ...string) ([]byte, error)
}

// ExecRunner runs the flatpak executable at Path.
type ExecRunner struct {
	Path    string
	Timeout time.Duration // per command, 0 for none
}

// Output runs the command with stdin closed, so that flatpak can never stop
// to ask a question, and folds the first line of its stderr into the error.
func (r ExecRunner) Output(ctx context.Context, argv ...string) ([]byte, error) {
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, r.Path, argv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if i := strings.IndexByte(msg, '\n'); i >= 0 {
			msg = msg[:i]
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("flatpak %s: %w", argv[0], ctx.Err())
		}
		if msg != "" {
			return nil, fmt.Errorf("flatpak %s: %s", argv[0], msg)
		}
		return nil, fmt.Errorf("flatpak %s: %w", argv[0], err)
	}
	return stdout.Bytes(), nil
}

// Plan is the outcome of planning.
type Plan struct {
	// Sources are unique by (checksum, name), in a stable order.
	Sources  []metadata.Source
	Warnings []string
}

// queryJobs bounds concurrent flatpak query processes; each remote-info may
// fetch a commit object from the remote.
const queryJobs = 4

type planner struct {
	ctx    context.Context
	runner Runner
	parsed args.Parsed

	mu       sync.Mutex
	warnings []string
}

// Build works out the extra-data sources of the refs the command will pull.
func Build(ctx context.Context, runner Runner, parsed args.Parsed) Plan {
	p := &planner{ctx: ctx, runner: runner, parsed: parsed}
	for _, name := range parsed.Unknown {
		p.warn("unknown option %s, assuming it takes no value", name)
	}
	if parsed.SkipReason != "" {
		return Plan{Warnings: p.warnings}
	}
	var results []result
	switch parsed.Command {
	case "update":
		results = p.update()
	case "install":
		results = p.install()
	}

	var plan Plan
	seen := make(map[[2]string]bool)
	for _, r := range results {
		sources, skipped := metadata.Parse(r.metadata, r.ref)
		for _, s := range skipped {
			p.warn("%s: not prefetching extra-data %s: %s", r.ref, s.Key, s.Reason)
		}
		for _, s := range sources {
			// Several refs, or the same ref in the user and the system
			// installation, can share a file; one copy serves all of them.
			key := [2]string{s.Checksum, s.Name}
			if !seen[key] {
				seen[key] = true
				plan.Sources = append(plan.Sources, s)
			}
		}
	}
	plan.Warnings = p.warnings
	return plan
}

func (p *planner) warn(format string, a ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.warnings = append(p.warnings, fmt.Sprintf(format, a...))
}

// target is a ref to look up with remote-info.
type target struct {
	installation string
	remote       string
	ref          string
}

// result is remote-info's output for a target.
type result struct {
	target
	metadata string
	err      error
}

func (p *planner) update() []result {
	perInstallation := parallel(p.parsed.Installations, func(installation string) []target {
		return p.pendingUpdates(installation)
	})
	var targets []target
	for _, ts := range perInstallation {
		for _, t := range ts {
			if matchesAny(t.ref, p.parsed.Positionals) {
				targets = append(targets, t)
			}
		}
	}
	results := p.remoteInfo(targets)
	var ok []result
	for _, r := range results {
		if r.err != nil {
			p.warn("cannot read %s from %s (%s): %v", r.ref, r.remote, r.installation, r.err)
			continue
		}
		ok = append(ok, r)
	}
	return ok
}

// pendingUpdates lists the refs of an installation that have updates.
func (p *planner) pendingUpdates(installation string) []target {
	argv := []string{"remote-ls", installation, "--updates", "--columns=ref,origin"}
	if p.parsed.Arch != "" {
		argv = append(argv, "--arch="+p.parsed.Arch)
	}
	out, err := p.runner.Output(p.ctx, argv...)
	if err == nil {
		return parseUpdates(installation, out)
	}
	// Without a REMOTE argument remote-ls gives up as soon as one remote
	// cannot be reached; retry remote by remote so the others still count.
	// If the installation itself is missing or unreadable, listing its
	// remotes fails too, and there is nothing to do for it.
	remotes, rerr := p.remotes(installation)
	if rerr != nil {
		if p.parsed.ExplicitInstallation {
			p.warn("cannot list updates of %s: %v", installation, err)
		}
		return nil
	}
	var targets []target
	for _, ts := range parallel(remotes, func(remote string) []target {
		out, err := p.runner.Output(p.ctx, append(argv, remote)...)
		if err != nil {
			p.warn("cannot list updates from %s (%s): %v", remote, installation, err)
			return nil
		}
		return parseUpdates(installation, out)
	}) {
		targets = append(targets, ts...)
	}
	return targets
}

// parseUpdates reads `remote-ls --columns=ref,origin` output. When stdout
// is not a terminal flatpak prints no header and separates columns by tabs
// (app/flatpak-table-printer.c).
func parseUpdates(installation string, out []byte) []target {
	var targets []target
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !isFullRef(fields[0]) {
			continue
		}
		targets = append(targets, target{installation: installation, remote: fields[1], ref: fields[0]})
	}
	return targets
}

func (p *planner) install() []result {
	installations := p.parsed.Installations
	remoteLists := parallel(installations, func(installation string) []string {
		remotes, err := p.remotes(installation)
		if err != nil && p.parsed.ExplicitInstallation {
			p.warn("cannot list remotes of %s: %v", installation, err)
		}
		return remotes
	})
	hasRemote := func(i int, name string) bool {
		for _, r := range remoteLists[i] {
			if r == name {
				return true
			}
		}
		return false
	}
	remote, refs := args.SplitInstallRemote(p.parsed.Positionals, func(name string) bool {
		for i := range installations {
			if hasRemote(i, name) {
				return true
			}
		}
		return false
	})
	// Only possible after "--"; such an argument cannot be a valid ref and
	// would be read as an option by remote-info.
	for _, ref := range refs {
		if strings.HasPrefix(ref, "-") {
			p.warn("%s: not a ref, leaving it to flatpak", ref)
			return nil
		}
	}

	if remote != "" {
		// Without an installation option flatpak asks which installation to
		// use if several have the remote; prefetch for all of them.
		var targets []target
		for _, ref := range refs {
			for i, installation := range installations {
				if hasRemote(i, remote) {
					targets = append(targets, target{installation, remote, ref})
				}
			}
		}
		var ok []result
		for _, r := range p.remoteInfo(targets) {
			if r.err != nil {
				p.warn("cannot read %s from %s (%s): %v", r.ref, r.remote, r.installation, r.err)
				continue
			}
			ok = append(ok, r)
		}
		return ok
	}

	// No REMOTE: flatpak searches every remote of every installation for the
	// first REF, asks the user to confirm or pick, and installs all REFs
	// from that remote. rpak uses a remote only when it is the only one that
	// has the ref; the same remote name in the user and the system
	// installation counts as one, both are prefetched.
	var ok []result
	preferred := ""
	for n, ref := range refs {
		var targets []target
		for i, installation := range installations {
			for _, r := range remoteLists[i] {
				targets = append(targets, target{installation, r, ref})
			}
		}
		byRemote := make(map[string][]result)
		var names []string
		for _, r := range p.remoteInfo(targets) {
			if r.err != nil {
				continue
			}
			if byRemote[r.remote] == nil {
				names = append(names, r.remote)
			}
			byRemote[r.remote] = append(byRemote[r.remote], r)
		}
		chosen := ""
		switch {
		case preferred != "" && byRemote[preferred] != nil:
			chosen = preferred
		case len(names) == 1:
			chosen = names[0]
		case len(names) == 0:
			p.warn("%s: not found in any remote, leaving it to flatpak", ref)
		default:
			p.warn("%s: found in several remotes (%s), leaving it to flatpak", ref, strings.Join(names, ", "))
		}
		if n == 0 {
			preferred = chosen
		}
		ok = append(ok, byRemote[chosen]...)
	}
	return ok
}

func (p *planner) remotes(installation string) ([]string, error) {
	out, err := p.runner.Output(p.ctx, "remotes", installation, "--columns=name")
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// remoteInfo runs `flatpak remote-info --show-ref --show-metadata` for all
// targets, a few at a time, and returns the results in target order.
func (p *planner) remoteInfo(targets []target) []result {
	return parallel(targets, func(t target) result {
		argv := []string{"remote-info", t.installation, "--show-ref", "--show-metadata"}
		if p.parsed.Arch != "" {
			argv = append(argv, "--arch="+p.parsed.Arch)
		}
		argv = append(argv, t.remote, t.ref)
		out, err := p.runner.Output(p.ctx, argv...)
		if err != nil {
			return result{target: t, err: err}
		}
		// --show-ref puts the resolved full ref on the first line, before
		// the metadata (flatpak_builtin_remote_info()).
		ref, rest, _ := strings.Cut(string(out), "\n")
		ref = strings.TrimSpace(ref)
		if !isFullRef(ref) {
			return result{target: t, err: errors.New("unexpected remote-info output")}
		}
		t.ref = ref
		return result{target: t, metadata: rest}
	})
}

// parallel maps fn over items with at most queryJobs running at once,
// keeping the order of items.
func parallel[T, R any](items []T, fn func(T) R) []R {
	out := make([]R, len(items))
	sem := make(chan struct{}, queryJobs)
	var wg sync.WaitGroup
	for i, item := range items {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			out[i] = fn(item)
		}()
	}
	wg.Wait()
	return out
}

func isFullRef(ref string) bool {
	parts := strings.Split(ref, "/")
	return len(parts) == 4 && (parts[0] == "app" || parts[0] == "runtime") &&
		parts[1] != "" && parts[2] != "" && parts[3] != ""
}

// matchesAny reports whether a full ref is selected by update's REF
// arguments; no arguments select everything. Matching is looser than
// flatpak's own (an extra download is harmless, a missed one is not).
func matchesAny(ref string, patterns []string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, pattern := range patterns {
		if matches(ref, pattern) {
			return true
		}
	}
	return false
}

// matches accepts a full ref, or a partial one in flatpak's
// ID[/ARCH[/BRANCH]] form where empty parts match anything
// (org.example.App, org.example.App//beta, app/org.example.App).
func matches(ref, pattern string) bool {
	if ref == pattern {
		return true
	}
	r := strings.Split(ref, "/") // kind, id, arch, branch
	p := strings.Split(pattern, "/")
	if p[0] == "app" || p[0] == "runtime" {
		if p[0] != r[0] {
			return false
		}
		p = p[1:]
	}
	if len(p) == 0 || len(p) > 3 || p[0] != r[1] {
		return false
	}
	for i := 1; i < len(p); i++ {
		if p[i] != "" && p[i] != r[i+1] {
			return false
		}
	}
	return true
}
