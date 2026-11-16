package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/flatpark/rpak/internal/prefetch"
)

var errNoFlatpak = errors.New("flatpak is not usable")

type basicsInfo struct {
	found   bool   // the flatpak executable exists
	self    bool   // and is rpak itself
	version string // flatpak --version
	versErr error
	arch    string
	archErr error
}

func (d *doctor) basicsStart() *basicsInfo {
	b := &basicsInfo{}
	if info, err := os.Stat(d.cfg.Flatpak); err == nil && filepath.IsAbs(d.cfg.Flatpak) && info.Mode().IsRegular() {
		b.found = true
		b.self = SameExecutable(d.cfg.Flatpak, d.cfg.Executable)
	}
	d.flatpak = b.found && !b.self && d.cfg.Runner != nil
	return b
}

func (b *basicsInfo) finish(d *doctor) {
	if !d.flatpak {
		return
	}
	parallel(
		func() {
			out, err := d.query("--version")
			b.version, b.versErr = strings.TrimSpace(out), err
		},
		func() {
			out, err := d.query("--default-arch")
			b.arch, b.archErr = strings.TrimSpace(out), err
		},
	)
}

// SameExecutable reports whether flatpak is rpak itself, through symlinks
// or hard links: running it would exec rpak again, forever.
func SameExecutable(flatpak, self string) bool {
	if flatpak == "" || self == "" {
		return false
	}
	a, errA := filepath.EvalSymlinks(flatpak)
	b, errB := filepath.EvalSymlinks(self)
	if errA == nil && errB == nil && a == b {
		return true
	}
	fa, errA := os.Stat(flatpak)
	fb, errB := os.Stat(self)
	return errA == nil && errB == nil && os.SameFile(fa, fb)
}

type releaseInfo struct {
	tag string
	err error
}

// latestRelease asks GitHub for the tag of rpak's latest release.
func (d *doctor) latestRelease() releaseInfo {
	if d.cfg.ReleaseURL == "" {
		return releaseInfo{err: errors.New("not checked")}
	}
	ctx, cancel := withTimeout(d.ctx, d.cfg.NetTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.cfg.ReleaseURL, nil)
	if err != nil {
		return releaseInfo{err: err}
	}
	req.Header.Set("User-Agent", prefetch.UserAgent(d.cfg.Version))
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := d.cfg.Client.Do(req)
	if err != nil {
		return releaseInfo{err: errors.New(errorClass(err))}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return releaseInfo{err: fmt.Errorf("HTTP %d", resp.StatusCode)}
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil || body.TagName == "" {
		return releaseInfo{err: errors.New("unexpected answer")}
	}
	return releaseInfo{tag: body.TagName}
}

func (d *doctor) basicsSection(b *basicsInfo, rel releaseInfo) Section {
	s := d.section("rpak and flatpak")
	latest := "latest release " + rel.tag
	if rel.err != nil {
		latest = "latest release: " + rel.err.Error()
	}
	s.add("rpak %s  %s  (%s)", d.cfg.Version, d.cfg.Executable, latest)

	what := "flatpak in PATH"
	if v, _ := d.cfg.LookupEnv("RPAK_FLATPAK"); v != "" {
		what = "RPAK_FLATPAK"
	}
	switch {
	case !b.found:
		path, _ := d.cfg.LookupEnv("PATH")
		s.add("flatpak: not found (%s: %s; PATH=%s)", what, d.cfg.Flatpak, path)
	case b.self:
		// Running it would run rpak again, forever: no flatpak command
		// is run at all.
		s.add("%s resolves to this rpak binary (%s); no flatpak command is run", what, d.cfg.Flatpak)
	case b.versErr != nil:
		s.add("flatpak --version: %v  %s", b.versErr, d.cfg.Flatpak)
	default:
		version, _ := strings.CutPrefix(b.version, "Flatpak ")
		s.add("flatpak %s  %s", version, d.cfg.Flatpak)
	}

	distro := osRelease(d.path("/etc/os-release"))
	if distro == "" {
		distro = osRelease(d.path("/usr/lib/os-release"))
	}
	if distro == "" {
		distro = "unknown"
	}
	s.add("distribution: %s", distro)
	if d.cfg.Kernel != nil {
		s.add("kernel: %s", d.cfg.Kernel())
	}
	switch {
	case b.archErr != nil:
		s.add("flatpak --default-arch: %v", b.archErr)
	case b.arch != "":
		s.add("architecture: %s", b.arch)
	}

	if sudoUser := d.getenv("SUDO_USER"); sudoUser != "" {
		s.add("sudo: SUDO_USER=%s, uid %d, euid %d", sudoUser, d.cfg.Uid, d.cfg.Euid)
	}
	return s.done()
}

// osRelease is PRETTY_NAME from an os-release file.
func osRelease(path string) string {
	text, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(text), "\n") {
		if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
			if u, err := strconv.Unquote(v); err == nil {
				return u
			}
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}
