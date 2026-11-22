package doctor

import (
	"net/http"
	"strings"

	"github.com/flatpark/rpak/internal/prefetch"
)

// The environment variables doctor shows; nothing else of the environment
// is printed. Those marked always are shown as "(unset)" when unset.
var envVars = []struct {
	name   string
	always bool
}{
	{"RPAK_FLATPAK", true}, {"RPAK_JOBS", true}, {"RPAK_DRY_RUN", true}, {"RPAK_RELEASE_BASE", false},
	{"FLATPAK_USER_DIR", false}, {"FLATPAK_SYSTEM_DIR", false}, {"FLATPAK_CONFIG_DIR", false},
	{"XDG_DATA_HOME", false}, {"XDG_DATA_DIRS", true},
	{"XDG_CURRENT_DESKTOP", true}, {"XDG_SESSION_TYPE", true}, {"DESKTOP_SESSION", false},
	{"WAYLAND_DISPLAY", true}, {"DISPLAY", true},
	{"LANG", true}, {"LC_ALL", false}, {"LANGUAGE", false},
	{"GTK_IM_MODULE", false}, {"QT_IM_MODULE", false}, {"XMODIFIERS", false},
}

// proxyVars are checked in both cases, as curl and Go read them.
var proxyVars = []string{"http_proxy", "https_proxy", "all_proxy", "no_proxy"}

func (d *doctor) envSection() Section {
	s := d.section("Environment")
	for _, v := range envVars {
		value, set := d.cfg.LookupEnv(v.name)
		if !set && !v.always {
			continue
		}
		shown := value
		if !set {
			shown = "(unset)"
		}
		if v.name == "RPAK_JOBS" {
			// What rpak makes of it, which is not always the value.
			n, _ := prefetch.ParseJobs(value)
			s.add("RPAK_JOBS=%s  (%d downloads at a time)", shown, n)
			continue
		}
		if v.name == "RPAK_RELEASE_BASE" {
			shown = RedactURL(shown) // a mirror may take a password
		}
		s.add("%s=%s", v.name, shown)
	}
	for _, name := range proxyVars {
		for _, n := range []string{name, strings.ToUpper(name)} {
			if value, set := d.cfg.LookupEnv(n); set {
				s.add("%s=%s", n, RedactURLList(value))
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
