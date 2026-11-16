// Package doctor collects what is needed to look into a problem report:
// rpak, flatpak and the system they run on, the installations, the
// network, and optionally single apps, with what looks wrong marked. It
// only reads: it takes no locks, writes no files and runs nothing but
// flatpak's query commands.
//
// Everything it depends on is in Config, so that each check can be tested
// on its own. Problems in one check only affect that check.
package doctor

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/flatpark/rpak/internal/installation"
	"github.com/flatpark/rpak/internal/plan"
	"github.com/flatpark/rpak/internal/prefetch"
)

// Level is how an item is marked.
type Level int

const (
	Info Level = iota // a fact, no judgement
	OK                // ✓
	Warn              // !: worth a look, may explain a problem
	Fail              // ✗: will make something fail
)

// Item is a line of the report.
type Item struct {
	Level Level
	Text  string
	// Hint says what to do about a Warn or Fail, in the Problems summary.
	Hint string
	// Block is verbatim text (a file, flatpak's output) shown below Text.
	Block string
	// Head marks a heading within the section (an installation); Context
	// of the items after it is its text.
	Head    bool
	Context string
}

// Section is a titled part of the report.
type Section struct {
	Title string
	Items []Item
}

// Report is the outcome of Run, in a fixed order.
type Report struct {
	Sections []Section
}

// Problems are the Warn and Fail items, in report order.
func (r Report) Problems() []Item {
	var out []Item
	for _, s := range r.Sections {
		for _, it := range s.Items {
			if it.Level == Warn || it.Level == Fail {
				out = append(out, it)
			}
		}
	}
	return out
}

// Failed reports whether there is a Fail item.
func (r Report) Failed() bool {
	for _, p := range r.Problems() {
		if p.Level == Fail {
			return true
		}
	}
	return false
}

// Config is what doctor looks at.
type Config struct {
	Version    string // rpak's
	Executable string // rpak's own path
	// Flatpak is the flatpak rpak runs, as found in PATH or $RPAK_FLATPAK;
	// Runner runs it. Runner is not used if Flatpak turns out to be rpak.
	Flatpak string
	Runner  plan.Runner
	Client  *http.Client
	// Proxy is http.ProxyFromEnvironment, or what stands for it in tests.
	Proxy     func(*http.Request) (*url.URL, error)
	LookupEnv func(string) (string, bool)
	Home      string
	// Users are the user names to hide; OtherHomes the homes of users
	// other than the current one (SUDO_USER) to hide, path → name.
	Users      []string
	OtherHomes map[string]string
	// Root is prepended to /proc, /sys, /etc and /usr paths; "" is the real root.
	Root   string
	Uid    int
	Euid   int
	Kernel func() string
	// ReleaseURL is the GitHub API URL of rpak's latest release.
	ReleaseURL string
	// Probes are the URLs always checked, besides the remotes.
	Probes []string
	Apps   []string

	// Arch is flatpak's name for the machine's architecture, for looking
	// up apps that are not installed.
	Arch string

	NetTimeout     time.Duration // per HTTP check
	SummaryTimeout time.Duration // fetching one remote's summary
	DuTimeout      time.Duration // measuring an app's data directory
	CleanTimeout   time.Duration // the clean dry run
}

// FlatpakTimeout bounds each flatpak command doctor runs.
const FlatpakTimeout = 15 * time.Second

// NewConfig is the configuration of the running process.
func NewConfig(version, flatpak string, apps []string) Config {
	exe, _ := os.Executable()
	home, _ := os.UserHomeDir()
	var users []string
	others := map[string]string{}
	if u, err := user.Current(); err == nil {
		users = append(users, u.Username)
	}
	if name := os.Getenv("SUDO_USER"); name != "" {
		users = append(users, name)
		if u, err := user.Lookup(name); err == nil && u.HomeDir != home {
			others[u.HomeDir] = name
		}
	}
	return Config{
		Version:        version,
		Executable:     exe,
		Flatpak:        flatpak,
		Runner:         plan.ExecRunner{Path: flatpak, Timeout: FlatpakTimeout},
		Client:         prefetch.NewHTTPClient(),
		Proxy:          http.ProxyFromEnvironment,
		LookupEnv:      os.LookupEnv,
		Home:           home,
		Users:          users,
		OtherHomes:     others,
		Uid:            os.Getuid(),
		Euid:           os.Geteuid(),
		Kernel:         kernelRelease,
		ReleaseURL:     "https://api.github.com/repos/flatpark/rpak/releases/latest",
		Probes:         []string{"https://dl.flatpark.org/flatpark.flatpakrepo", "https://dl.flathub.org/repo/summary"},
		Apps:           apps,
		Arch:           flatpakArch(),
		SummaryTimeout: 15 * time.Second,
		NetTimeout:     10 * time.Second,
		DuTimeout:      5 * time.Second,
		CleanTimeout:   5 * time.Second,
	}
}

func kernelRelease() string {
	var u unix.Utsname
	if unix.Uname(&u) != nil {
		return ""
	}
	return unix.ByteSliceToString(u.Release[:])
}

// doctor is one run.
type doctor struct {
	cfg Config
	r   Redactor
	ctx context.Context
	// flatpak is false when flatpak is missing or is rpak: nothing is run.
	flatpak bool
	cache   summaries
}

func (d *doctor) getenv(name string) string {
	v, _ := d.cfg.LookupEnv(name)
	return v
}

func (d *doctor) path(p string) string { return filepath.Join(d.cfg.Root, p) }

func (d *doctor) installEnv() installation.Env {
	return installation.Env{LookupEnv: d.cfg.LookupEnv, HomeDir: func() (string, error) { return d.cfg.Home, nil }}
}

// query runs a flatpak query command, unless flatpak is not usable.
func (d *doctor) query(argv ...string) (string, error) {
	if !d.flatpak {
		return "", errNoFlatpak
	}
	out, err := d.cfg.Runner.Output(d.ctx, argv...)
	return string(out), err
}

// parallel runs the functions at the same time and waits for them.
func parallel(fns ...func()) {
	var wg sync.WaitGroup
	for _, fn := range fns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn()
		}()
	}
	wg.Wait()
}

// Run collects everything, as concurrently as the dependencies between
// the checks allow, and returns the report in its fixed order.
func Run(ctx context.Context, cfg Config) Report {
	d := &doctor{cfg: cfg, ctx: ctx, r: NewRedactor(cfg.Home, cfg.OtherHomes, cfg.Users)}
	if d.cfg.LookupEnv == nil {
		d.cfg.LookupEnv = os.LookupEnv
	}
	if d.cfg.Proxy == nil {
		d.cfg.Proxy = http.ProxyFromEnvironment
	}
	if d.cfg.Client == nil {
		d.cfg.Client = http.DefaultClient
	}
	if d.cfg.Arch == "" {
		d.cfg.Arch = flatpakArch()
	}

	basics := d.basicsStart() // decides whether flatpak may run
	var (
		insts   []*instInfo
		release releaseInfo
		system  systemInfo
		extra   extraDataInfo
		network []probeResult
		apps    []*appInfo
		ps      psInfo
	)
	parallel(
		func() { basics.finish(d) },
		func() { release = d.latestRelease() },
		func() { system = d.systemInfo() },
		func() {
			insts = d.installations()
			parallel(
				func() { network = d.network(insts) },
				func() { extra = d.extraData(insts) },
				func() { apps = d.apps(insts) },
			)
		},
		func() { ps = d.ps() },
	)

	return Report{Sections: []Section{
		d.basicsSection(basics, release),
		d.envSection(insts),
		d.systemSection(system, insts),
		d.installationsSection(insts, apps),
		d.extraDataSection(extra),
		d.networkSection(network),
	}}.withApps(d, apps, insts, ps)
}

func (r Report) withApps(d *doctor, apps []*appInfo, insts []*instInfo, ps psInfo) Report {
	for _, a := range apps {
		r.Sections = append(r.Sections, d.appSection(a, insts, ps))
	}
	return r
}

// builder collects the items of a section, giving each the context of the
// heading before it.
type builder struct {
	r       Redactor
	title   string
	items   []Item
	context string
}

func (d *doctor) section(title string) *builder {
	return &builder{r: d.r, title: title, context: title}
}

// head starts a part of the section; context names it in the Problems
// summary.
func (b *builder) head(context, format string, a ...any) {
	b.context = b.r.String(context)
	b.items = append(b.items, Item{Head: true, Text: b.r.Sprintf(format, a...)})
}

// add appends an item with a redacted text; hint (for Warn and Fail) and
// block are set with the returned pointer's methods.
func (b *builder) add(level Level, format string, a ...any) *Item {
	b.items = append(b.items, Item{Level: level, Text: b.r.Sprintf(format, a...), Context: b.context})
	return &b.items[len(b.items)-1]
}

func (b *builder) done() Section { return Section{Title: b.title, Items: b.items} }

// hint sets the hint; its arguments are redacted, its format is not.
func (it *Item) hint(r Redactor, format string, a ...any) *Item {
	it.Hint = r.Sprintf(format, a...)
	return it
}

func (it *Item) block(r Redactor, text string) *Item {
	it.Block = r.String(text)
	return it
}
