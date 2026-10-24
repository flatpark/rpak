package plan

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/flatpark/rpak/internal/args"
)

// fakeRunner answers flatpak queries from a table keyed by the joined argv.
type fakeRunner struct {
	answers map[string]string

	mu    sync.Mutex
	calls []string
}

func (f *fakeRunner) Output(_ context.Context, argv ...string) ([]byte, error) {
	key := strings.Join(argv, " ")
	f.mu.Lock()
	f.calls = append(f.calls, key)
	f.mu.Unlock()
	if out, ok := f.answers[key]; ok {
		return []byte(out), nil
	}
	return nil, errors.New("not found")
}

func extraData(ref, name string, sum byte) string {
	return ref + "\n[Application]\nname=x\n\n[Extra Data]\nname=" + name +
		"\nchecksum=" + strings.Repeat(string(sum), 64) +
		"\nsize=10\nuri=https://example.com/" + name + "\n"
}

func names(p Plan) []string {
	var out []string
	for _, s := range p.Sources {
		out = append(out, s.Name)
	}
	sort.Strings(out)
	return out
}

const (
	chrome = "app/com.google.Chrome/x86_64/stable"
	zoom   = "app/us.zoom.Zoom/x86_64/stable"
	plain  = "app/org.example.Plain/x86_64/stable"
)

func TestUpdate(t *testing.T) {
	runner := &fakeRunner{answers: map[string]string{
		"remote-ls --user --updates --columns=ref,origin":                    chrome + "\tflatpark\n" + plain + "\tflathub\n",
		"remote-ls --system --updates --columns=ref,origin":                  zoom + "\tflatpark\n" + chrome + "\tflatpark\n",
		"remote-info --user --show-ref --show-metadata flatpark " + chrome:   extraData(chrome, "chrome.deb", 'a'),
		"remote-info --system --show-ref --show-metadata flatpark " + chrome: extraData(chrome, "chrome.deb", 'a'),
		"remote-info --system --show-ref --show-metadata flatpark " + zoom:   extraData(zoom, "zoom.deb", 'b'),
		"remote-info --user --show-ref --show-metadata flathub " + plain:     plain + "\n[Application]\nname=x\n",
	}}
	p := Build(context.Background(), runner, args.Parse("update", nil))
	if got, want := names(p), []string{"chrome.deb", "zoom.deb"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sources = %q, want %q (warnings %q)", got, want, p.Warnings)
	}
	if len(p.Warnings) != 0 {
		t.Errorf("warnings = %q", p.Warnings)
	}

	// REF arguments filter the updates: app id, partial and full refs.
	for _, filter := range []string{"us.zoom.Zoom", "us.zoom.Zoom//stable", zoom, "app/us.zoom.Zoom"} {
		p = Build(context.Background(), runner, args.Parse("update", []string{"-y", filter}))
		if got := names(p); !reflect.DeepEqual(got, []string{"zoom.deb"}) {
			t.Errorf("filter %q: sources = %q", filter, got)
		}
	}
	p = Build(context.Background(), runner, args.Parse("update", []string{"us.zoom.Zoom//beta", "runtime/us.zoom.Zoom"}))
	if len(p.Sources) != 0 {
		t.Errorf("non-matching filters: sources = %q", names(p))
	}
}

func TestUpdateInstallationOptionsAndArch(t *testing.T) {
	runner := &fakeRunner{answers: map[string]string{
		"remote-ls --installation=extra --updates --columns=ref,origin --arch=aarch64":                                             "app/com.google.Chrome/aarch64/stable\tflatpark\n",
		"remote-info --installation=extra --show-ref --show-metadata --arch=aarch64 flatpark app/com.google.Chrome/aarch64/stable": extraData("app/com.google.Chrome/aarch64/stable", "chrome.deb", 'c'),
	}}
	p := Build(context.Background(), runner, args.Parse("update", []string{"--installation", "extra", "--arch=aarch64"}))
	if got := names(p); !reflect.DeepEqual(got, []string{"chrome.deb"}) {
		t.Fatalf("sources = %q, calls %q", got, runner.calls)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "--user") || strings.Contains(call, "--system") {
			t.Errorf("queried another installation: %q", call)
		}
	}
}

func TestUpdateFallsBackToPerRemoteListing(t *testing.T) {
	// remote-ls without REMOTE fails when one remote is unreachable.
	runner := &fakeRunner{answers: map[string]string{
		"remotes --user --columns=name":                                    "flatpark\nbroken\n",
		"remote-ls --user --updates --columns=ref,origin flatpark":         chrome + "\tflatpark\n",
		"remote-info --user --show-ref --show-metadata flatpark " + chrome: extraData(chrome, "chrome.deb", 'a'),
	}}
	p := Build(context.Background(), runner, args.Parse("update", []string{"--user"}))
	if got := names(p); !reflect.DeepEqual(got, []string{"chrome.deb"}) {
		t.Fatalf("sources = %q, calls %q", got, runner.calls)
	}
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "broken") {
		t.Errorf("warnings = %q", p.Warnings)
	}
}

func TestUpdateMissingInstallationIsSilent(t *testing.T) {
	// No system installation at all: nothing to do, nothing to warn about.
	runner := &fakeRunner{answers: map[string]string{
		"remote-ls --user --updates --columns=ref,origin": "",
	}}
	p := Build(context.Background(), runner, args.Parse("update", nil))
	if len(p.Sources) != 0 || len(p.Warnings) != 0 {
		t.Fatalf("plan = %#v", p)
	}
}

func TestInstallWithRemote(t *testing.T) {
	runner := &fakeRunner{answers: map[string]string{
		"remotes --user --columns=name":                                            "flathub\nflatpark\n",
		"remotes --system --columns=name":                                          "flathub\n",
		"remote-info --user --show-ref --show-metadata flatpark com.google.Chrome": extraData(chrome, "chrome.deb", 'a'),
		"remote-info --user --show-ref --show-metadata flatpark us.zoom.Zoom":      extraData(zoom, "zoom.deb", 'b'),
	}}
	p := Build(context.Background(), runner, args.Parse("install", []string{"-y", "flatpark", "com.google.Chrome", "us.zoom.Zoom"}))
	if got := names(p); !reflect.DeepEqual(got, []string{"chrome.deb", "zoom.deb"}) {
		t.Fatalf("sources = %q, warnings %q", got, p.Warnings)
	}
	// The system installation has no "flatpark" remote: never asked.
	for _, call := range runner.calls {
		if strings.HasPrefix(call, "remote-info --system") {
			t.Errorf("unexpected call %q", call)
		}
	}
	if p.Sources[0].Ref != chrome {
		t.Errorf("ref = %q, want the full ref from --show-ref", p.Sources[0].Ref)
	}
}

func TestInstallSameRemoteInBothInstallations(t *testing.T) {
	// Different commits in user and system: both files are prefetched.
	runner := &fakeRunner{answers: map[string]string{
		"remotes --user --columns=name":                                              "flatpark\n",
		"remotes --system --columns=name":                                            "flatpark\n",
		"remote-info --user --show-ref --show-metadata flatpark com.google.Chrome":   extraData(chrome, "chrome.deb", 'a'),
		"remote-info --system --show-ref --show-metadata flatpark com.google.Chrome": extraData(chrome, "chrome.deb", 'b'),
	}}
	for _, argv := range [][]string{{"flatpark", "com.google.Chrome"}, {"com.google.Chrome"}} {
		p := Build(context.Background(), runner, args.Parse("install", argv))
		if len(p.Sources) != 2 {
			t.Errorf("%q: sources = %#v", argv, p.Sources)
		}
	}
}

func TestInstallWithoutRemote(t *testing.T) {
	answers := map[string]string{
		"remotes --user --columns=name":                                            "flathub\nflatpark\n",
		"remotes --system --columns=name":                                          "flathub\nflatpark\n",
		"remote-info --user --show-ref --show-metadata flatpark com.google.Chrome": extraData(chrome, "chrome.deb", 'a'),
		// Ambiguous: Zoom exists in two remotes.
		"remote-info --user --show-ref --show-metadata flatpark us.zoom.Zoom": extraData(zoom, "zoom.deb", 'b'),
		"remote-info --user --show-ref --show-metadata flathub us.zoom.Zoom":  extraData(zoom, "zoom-flathub.deb", 'c'),
	}
	runner := &fakeRunner{answers: answers}

	p := Build(context.Background(), runner, args.Parse("install", []string{"com.google.Chrome"}))
	if got := names(p); !reflect.DeepEqual(got, []string{"chrome.deb"}) {
		t.Errorf("unique remote: sources = %q", got)
	}

	p = Build(context.Background(), runner, args.Parse("install", []string{"us.zoom.Zoom"}))
	if len(p.Sources) != 0 || len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "several remotes") {
		t.Errorf("ambiguous remote: plan = %#v", p)
	}

	p = Build(context.Background(), runner, args.Parse("install", []string{"org.missing.App"}))
	if len(p.Sources) != 0 || len(p.Warnings) != 1 {
		t.Errorf("missing ref: plan = %#v", p)
	}

	// flatpak installs every REF from the remote chosen for the first one.
	p = Build(context.Background(), runner, args.Parse("install", []string{"com.google.Chrome", "us.zoom.Zoom"}))
	if got := names(p); !reflect.DeepEqual(got, []string{"chrome.deb", "zoom.deb"}) {
		t.Errorf("remote from first ref: sources = %q", got)
	}
}

func TestInstallSingleArgumentIsARef(t *testing.T) {
	runner := &fakeRunner{answers: map[string]string{
		"remotes --user --columns=name":                                   "flatpark\n",
		"remote-info --user --show-ref --show-metadata flatpark flatpark": extraData("app/flatpark/x86_64/stable", "f.bin", 'a'),
	}}
	p := Build(context.Background(), runner, args.Parse("install", []string{"--user", "flatpark"}))
	if got := names(p); !reflect.DeepEqual(got, []string{"f.bin"}) {
		t.Errorf("sources = %q", got)
	}
}

func TestSkipRunsNoQueries(t *testing.T) {
	runner := &fakeRunner{}
	p := Build(context.Background(), runner, args.Parse("install", []string{"--from", "x.flatpakref"}))
	if len(p.Sources) != 0 || len(runner.calls) != 0 {
		t.Fatalf("plan = %#v, calls = %q", p, runner.calls)
	}
}

func TestUnknownOptionWarnsOnceAndStillPlans(t *testing.T) {
	runner := &fakeRunner{answers: map[string]string{
		"remote-ls --user --updates --columns=ref,origin":                  chrome + "\tflatpark\n",
		"remote-info --user --show-ref --show-metadata flatpark " + chrome: extraData(chrome, "chrome.deb", 'a'),
	}}
	p := Build(context.Background(), runner, args.Parse("update", []string{"--user", "--shiny", "--shiny"}))
	if len(p.Sources) != 1 || len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "--shiny") {
		t.Fatalf("plan = %#v", p)
	}
}

func TestInvalidExtraDataIsWarnedAndSkipped(t *testing.T) {
	bad := chrome + "\n[Extra Data]\nchecksum=" + strings.Repeat("a", 64) + "\nsize=1\nuri=https://example.com/get?id=1\n"
	runner := &fakeRunner{answers: map[string]string{
		"remote-ls --user --updates --columns=ref,origin":                  chrome + "\tflatpark\n",
		"remote-info --user --show-ref --show-metadata flatpark " + chrome: bad,
	}}
	p := Build(context.Background(), runner, args.Parse("update", []string{"--user"}))
	if len(p.Sources) != 0 || len(p.Warnings) != 1 {
		t.Fatalf("plan = %#v", p)
	}
}

func TestMatches(t *testing.T) {
	ref := "app/org.example.App/x86_64/stable"
	yes := []string{ref, "org.example.App", "org.example.App/x86_64", "org.example.App//stable", "org.example.App/x86_64/stable", "app/org.example.App", "app/org.example.App//stable"}
	no := []string{"org.example", "org.example.App/aarch64", "org.example.App//beta", "runtime/org.example.App", "org.example.App/x86_64/stable/x", "x/org.example.App"}
	for _, p := range yes {
		if !matches(ref, p) {
			t.Errorf("%q should match", p)
		}
	}
	for _, p := range no {
		if matches(ref, p) {
			t.Errorf("%q should not match", p)
		}
	}
}
