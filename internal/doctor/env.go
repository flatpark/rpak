package doctor

import (
	"net/http"
	"path/filepath"
	"strings"

	"github.com/flatpark/rpak/internal/prefetch"
)

// The environment variables doctor shows; nothing else of the environment
// is printed. Those marked always are shown as "(unset)" when unset.
var envVars = []struct {
	name   string
	always bool
}{
	{"RPAK_FLATPAK", true}, {"RPAK_JOBS", true}, {"RPAK_DRY_RUN", true},
	{"FLATPAK_USER_DIR", false}, {"FLATPAK_SYSTEM_DIR", false}, {"FLATPAK_CONFIG_DIR", false},
	{"XDG_DATA_HOME", false}, {"XDG_DATA_DIRS", true},
	{"XDG_CURRENT_DESKTOP", true}, {"XDG_SESSION_TYPE", true}, {"DESKTOP_SESSION", false},
	{"WAYLAND_DISPLAY", true}, {"DISPLAY", true},
	{"LANG", true}, {"LC_ALL", false}, {"LANGUAGE", false},
	{"GTK_IM_MODULE", false}, {"QT_IM_MODULE", false}, {"XMODIFIERS", false},
}

// proxyVars are checked in both cases, as curl and Go read them.
var proxyVars = []string{"http_proxy", "https_proxy", "all_proxy", "no_proxy"}

// JobsCheck judges RPAK_JOBS: the number of downloads rpak will run at a
// time, and whether the value is one rpak takes.
func JobsCheck(v string) (n int, ok bool) { return prefetch.ParseJobs(v) }

// DryRun reports whether RPAK_DRY_RUN makes install and update only show
// what they would do (app.prefetchPhase: set, and not "0").
func DryRun(v string) bool { return v != "" && v != "0" }

// MissingDataDirs returns the directories of wanted that XDG_DATA_DIRS
// lacks; set is false when it is unset, which the XDG spec reads as
// /usr/local/share:/usr/share.
func MissingDataDirs(value string, set bool, wanted []string) []string {
	if !set || value == "" {
		value = "/usr/local/share/:/usr/share/"
	}
	have := map[string]bool{}
	for _, dir := range strings.Split(value, ":") {
		if dir != "" {
			have[filepath.Clean(dir)] = true
		}
	}
	var missing []string
	for _, w := range wanted {
		if !have[filepath.Clean(w)] {
			missing = append(missing, w)
		}
	}
	return missing
}

func (d *doctor) envSection(insts []*instInfo) Section {
	s := d.section("Environment")
	r := d.r
	for _, v := range envVars {
		value, set := d.cfg.LookupEnv(v.name)
		if !set && !v.always {
			continue
		}
		shown := value
		if !set {
			shown = "(unset)"
		}
		switch v.name {
		case "RPAK_JOBS":
			if n, ok := JobsCheck(value); ok {
				s.add(Info, "RPAK_JOBS=%s  (%d downloads at a time)", shown, n)
			} else {
				s.add(Warn, "RPAK_JOBS=%s is not a number from 1 to %d; rpak runs %d downloads at a time", value, prefetch.MaxJobs, n).
					hint(r, "set RPAK_JOBS to a number from 1 to %d, or unset it", prefetch.MaxJobs)
			}
		case "RPAK_DRY_RUN":
			if DryRun(value) {
				s.add(Warn, "RPAK_DRY_RUN=%s: rpak does not really install or update anything", value).
					hint(r, "unset RPAK_DRY_RUN")
			} else {
				s.add(Info, "RPAK_DRY_RUN=%s", shown)
			}
		case "XDG_DATA_DIRS":
			s.add(Info, "XDG_DATA_DIRS=%s", shown)
			var wanted []string
			for _, in := range insts {
				if in.apps.err == nil && len(in.apps.refs) > 0 {
					wanted = append(wanted, filepath.Join(in.inst.Path, "exports", "share"))
				}
			}
			for _, dir := range MissingDataDirs(value, set, wanted) {
				s.add(Warn, "XDG_DATA_DIRS lacks %s: its apps do not show up in the menu", dir).
					hint(r, "log out and in again; if that does not help, check /etc/profile.d/flatpak.sh (https://flatpark.org/docs/users/setup/)")
			}
		default:
			s.add(Info, "%s=%s", v.name, shown)
		}
	}
	for _, name := range proxyVars {
		for _, n := range []string{name, strings.ToUpper(name)} {
			if value, set := d.cfg.LookupEnv(n); set {
				s.add(Info, "%s=%s", n, RedactURLList(value))
			}
		}
	}
	return s.done()
}

// RedactURLList redacts a proxy variable, which may also be a no_proxy
// list.
func RedactURLList(v string) string {
	if !strings.Contains(v, "@") {
		return v
	}
	parts := strings.Split(v, ",")
	for i, p := range parts {
		parts[i] = RedactURL(strings.TrimSpace(p))
	}
	return strings.Join(parts, ",")
}

// proxyFor is the proxy the HTTP client uses for a URL, redacted.
func (d *doctor) proxyFor(rawURL string) string {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return "?"
	}
	u, err := d.cfg.Proxy(req)
	switch {
	case err != nil:
		return "invalid: " + err.Error()
	case u == nil:
		return "none"
	}
	return RedactURL(u.String())
}
