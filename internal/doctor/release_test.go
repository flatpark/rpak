package doctor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLatestRelease(t *testing.T) {
	hang := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			http.Redirect(w, r, "/releases/tag/v0.3.6", http.StatusFound)
		case "/slow/latest":
			<-hang
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	defer close(hang)

	line := func(version string, selfUpdate bool, base string) string {
		t.Helper()
		d := &doctor{ctx: context.Background(), r: NewRedactor("", nil, nil), cfg: Config{
			Version: version, Executable: "/bin/rpak", SelfUpdate: selfUpdate,
			ReleaseBase: base, Client: srv.Client(), NetTimeout: 100 * time.Millisecond,
			Root: t.TempDir(), LookupEnv: func(string) (string, bool) { return "", false },
		}}
		return d.basicsSection(&basicsInfo{}, d.latestRelease()).Items[0].Text
	}
	base := srv.URL + "/releases"
	for _, c := range []struct {
		version    string
		selfUpdate bool
		base       string
		want       string
	}{
		// A newer release, and the command that installs it.
		{"v0.3.5", true, base, "rpak v0.3.5  /bin/rpak  (latest release v0.3.6, newer: rpak self-update)"},
		// Installed by a package manager: rpak self-update refuses.
		{"v0.3.5", false, base, "rpak v0.3.5  /bin/rpak  (latest release v0.3.6, newer)"},
		// Up to date, or newer than the latest.
		{"v0.3.6", true, base, "rpak v0.3.6  /bin/rpak  (latest release v0.3.6)"},
		{"v0.3.7", true, base, "rpak v0.3.7  /bin/rpak  (latest release v0.3.6)"},
		// Development builds, older or not.
		{"v0.3.5-3-gabc1234", true, base, "rpak v0.3.5-3-gabc1234  /bin/rpak  (latest release v0.3.6; this is a development build)"},
		{"v0.3.6-dirty", true, base, "rpak v0.3.6-dirty  /bin/rpak  (latest release v0.3.6; this is a development build)"},
		{"dev", true, base, "rpak dev  /bin/rpak  (latest release v0.3.6; this is a development build)"},
		// What went wrong.
		{"v0.3.5", true, "", "rpak v0.3.5  /bin/rpak  (latest release: not checked)"},
		{"v0.3.5", true, srv.URL + "/none", "rpak v0.3.5  /bin/rpak  (latest release: HTTP 404 from " + srv.URL + "/none/latest, not a redirect to the release)"},
		{"v0.3.5", true, srv.URL + "/slow", "rpak v0.3.5  /bin/rpak  (latest release: timeout)"},
	} {
		if got := line(c.version, c.selfUpdate, c.base); got != c.want {
			t.Errorf("%s, self-update %v, %s:\ngot  %s\nwant %s", c.version, c.selfUpdate, c.base, got, c.want)
		}
	}
}
