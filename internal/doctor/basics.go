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

// parseVersion reads a release version, "v1.2.3", or what git describe
// makes of a later commit, "v1.2.3-4-gabcdef[-dirty]" (ahead is then
// true). Anything else, "dev" or a bare commit, is not a release.
func parseVersion(v string) (nums []int, ahead bool, ok bool) {
	v = strings.TrimPrefix(v, "v")
	base, rest, _ := strings.Cut(v, "-")
	if base == "" {
		return nil, false, false
	}
	for _, part := range strings.Split(base, ".") {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return nil, false, false
		}
		nums = append(nums, n)
	}
	return nums, rest != "" && rest != "dirty", true
}

// Behind reports whether version is older than the release latest; ok is
// false if either is not a release version.
func Behind(version, latest string) (behind, ok bool) {
	cur, _, ok1 := parseVersion(version)
	rel, _, ok2 := parseVersion(latest)
	if !ok1 || !ok2 {
		return false, false
	}
	for i := 0; i < max(len(cur), len(rel)); i++ {
		var a, b int
		if i < len(cur) {
			a = cur[i]
		}
		if i < len(rel) {
			b = rel[i]
		}
		if a != b {
			return a < b, true
		}
	}
	return false, true
}

func (d *doctor) basicsSection(b *basicsInfo, rel releaseInfo) Section {
	s := d.section("rpak and flatpak")
	r := d.r
	s.add(Info, "rpak %s  %s", d.cfg.Version, d.cfg.Executable)
	switch behind, ok := Behind(d.cfg.Version, rel.tag); {
	case rel.err != nil:
		s.add(Info, "latest release: unknown (%v)", rel.err)
	case !ok:
		s.add(Info, "latest release: %s (this is not a release build)", rel.tag)
	case behind:
		s.add(Warn, "rpak %s is older than the latest release %s", d.cfg.Version, rel.tag).
			hint(r, "download the new release: https://github.com/flatpark/rpak/releases/latest")
	default:
		s.add(OK, "rpak is up to date (latest release %s)", rel.tag)
	}

	flatpakVar, _ := d.cfg.LookupEnv("RPAK_FLATPAK")
	switch {
	case !b.found:
		s.add(Fail, "flatpak not found (%s)", d.cfg.Flatpak).
			hint(r, "install flatpak from your distribution, or point RPAK_FLATPAK at it")
	case b.self:
		what := "flatpak in PATH"
		if flatpakVar != "" {
			what = "RPAK_FLATPAK"
		}
		s.add(Fail, "%s is rpak itself (%s): rpak would run itself forever", what, d.cfg.Flatpak).
			hint(r, "point RPAK_FLATPAK at the real flatpak (RPAK_FLATPAK=/usr/bin/flatpak); use \"alias flatpak=rpak\" instead of a link named flatpak")
	case b.versErr != nil:
		s.add(Fail, "flatpak %s does not run: %v", d.cfg.Flatpak, b.versErr).
			hint(r, "reinstall flatpak, or point RPAK_FLATPAK at a working one")
	default:
		s.add(OK, "%s  %s", b.version, d.cfg.Flatpak)
	}

	distro := osRelease(d.path("/etc/os-release"))
	if distro == "" {
		distro = osRelease(d.path("/usr/lib/os-release"))
	}
	if distro == "" {
		distro = "unknown"
	}
	s.add(Info, "distribution: %s", distro)
	if d.cfg.Kernel != nil {
		s.add(Info, "kernel: %s", d.cfg.Kernel())
	}
	if b.arch != "" {
		s.add(Info, "architecture: %s", b.arch)
	}

	if sudoUser := d.getenv("SUDO_USER"); sudoUser != "" && d.cfg.Euid == 0 {
		s.add(Warn, "running under sudo (as root for %s): the user installation and the extra-data directory looked at are root's", sudoUser).
			hint(r, "run \"rpak doctor\" without sudo, unless the problem is with the system installation only")
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
