package extradata

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/flatpark/rpak/internal/metadata"
)

var (
	sumA = strings.Repeat("a", 64)
	sumB = strings.Repeat("b", 64)
	sumC = strings.Repeat("c", 64)
	sumN = strings.Repeat("e", 64)
)

func TestParseSpec(t *testing.T) {
	for _, c := range []struct {
		in   string
		want Spec
		bad  bool
	}{
		{in: "https://example.com/a.deb", want: Spec{URL: "https://example.com/a.deb"}},
		{in: "code.deb=https://example.com/a.deb", want: Spec{Name: "code.deb", URL: "https://example.com/a.deb"}},
		// The query's "=" is the URL's.
		{in: "https://download2.interactivebrokers.com/x.sh?build=4.1", want: Spec{URL: "https://download2.interactivebrokers.com/x.sh?build=4.1"}},
		{in: "ntws.sh=https://h/x.sh?build=4.1", want: Spec{Name: "ntws.sh", URL: "https://h/x.sh?build=4.1"}},
		{in: "http://127.0.0.1:8765/p.txt", want: Spec{URL: "http://127.0.0.1:8765/p.txt"}},
		{in: "", bad: true},
		{in: "code.deb", bad: true},
		{in: "code.deb=", bad: true},
		{in: "=https://example.com/a.deb", bad: true},
		{in: "..=https://example.com/a.deb", bad: true},
		{in: "file:///tmp/a.deb", bad: true},
		{in: "ftp://example.com/a.deb", bad: true},
		{in: "https:///a.deb", bad: true},
	} {
		got, err := ParseSpec(c.in)
		if (err != nil) != c.bad || (!c.bad && got != c.want) {
			t.Errorf("ParseSpec(%q) = %+v, %v", c.in, got, err)
		}
	}
}

// sources of a few real flatpark apps (x86_64), as their metadata has them.
func src(name, uri string) metadata.Source {
	return metadata.Source{Name: name, URI: uri, Checksum: sumA, Size: 1}
}

var (
	vscode = []metadata.Source{
		src("code.deb", "https://vscode.download.prss.microsoft.com/dbazure/download/stable/8a14f0cc1b08d531eb83255ea3764ae22aa932b5/code_1.147.0-1794903003_amd64.deb"),
		src("host-spawn", "https://github.com/1player/host-spawn/releases/download/v1.6.2/host-spawn-x86_64"),
	}
	heidisql = []metadata.Source{
		src("libqt6pas.deb", "https://github.com/davidbannon/libqt6pas/releases/download/v6.2.10/libqt6pas6_6.2.10-1_amd64.deb"),
		src("sql-clients.tar.xz", "https://github.com/flatpark/prebuilt/releases/download/sql-clients-v1/sql-clients-sql-clients-v1-fd-25.08-x86_64.tar.xz"),
		src("heidisql.tgz", "https://github.com/HeidiSQL/HeidiSQL/releases/download/v12.22/build-qt6-v12.22.tgz"),
	}
	ibkr = []metadata.Source{
		src("krb5-gss.tar.xz", "https://github.com/flatpark/prebuilt/releases/download/krb5-gss-v2/krb5-gss-krb5-gss-v2-fd-26.08-x86_64.tar.xz"),
		src("ntws-installer.sh", "https://download2.interactivebrokers.com/installers/ntws/latest-standalone/ntws-latest-standalone-linux-x64.sh?build=4.1c20261110230520"),
		src("zulu-jre.tar.gz", "https://cdn.azul.com/zulu/bin/zulu17.66.19-ca-jre17.0.19-linux_x64.tar.gz"),
	}
	// Two files of one GitHub project.
	audiocpp = []metadata.Source{
		src("audio.tar.gz", "https://github.com/0xShug0/audio.cpp/releases/download/v0.9.5/audio-v0.9.5-bin-ubuntu-x64-cuda12.8-colab.tar.gz"),
		src("cudart.tar.gz", "https://github.com/0xShug0/audio.cpp/releases/download/v0.9.5/audio-v0.9.5-cudart-ubuntu-x64-cuda12.8.tar.gz"),
		src("nccl.deb", "https://developer.download.nvidia.com/compute/cuda/repos/ubuntu2204/x86_64/libnccl2_2.26.2-1+cuda12.8_amd64.deb"),
	}
	// Two Debian packages beside the app; flatpark/prebuilt three times.
	stirling = []metadata.Source{
		src("stirling-pdf.deb", "https://github.com/Stirling-Tools/Stirling-PDF/releases/download/v3.4.0/Stirling-PDF-linux-x86_64.deb"),
		src("appimage-tools.tar.xz", "https://github.com/flatpark/prebuilt/releases/download/appimage-tools-v2/appimage-tools-appimage-tools-v2-fd-26.08-x86_64.tar.xz"),
		src("tesseract.tar.xz", "https://github.com/flatpark/prebuilt/releases/download/tesseract-v2/tesseract-tesseract-v2-gnome-51-x86_64.tar.xz"),
		src("leptonica.tar.xz", "https://github.com/flatpark/prebuilt/releases/download/leptonica-v2/leptonica-leptonica-v2-gnome-51-x86_64.tar.xz"),
		src("libidn12.deb", "https://deb.debian.org/debian/pool/main/libi/libidn/libidn12_1.44-1_amd64.deb"),
		src("libpaper2.deb", "https://deb.debian.org/debian/pool/main/libp/libpaper/libpaper2_2.3.0-0.3_amd64.deb"),
	}
	cellar = []metadata.Source{
		src("cellar-x86_64", "https://github.com/jing2uo/cellar/releases/download/v0.5.2/cellar-x86_64"),
	}
)

func TestChoose(t *testing.T) {
	for _, c := range []struct {
		desc    string
		sources []metadata.Source
		spec    Spec
		want    string // "" for a ChoiceError
		auto    bool
	}{
		// 1. NAME given
		{"by name", stirling, Spec{Name: "libpaper2.deb", URL: "https://example.com/x.deb"}, "libpaper2.deb", false},
		{"by name, even if another URL is closer", vscode, Spec{Name: "host-spawn", URL: "https://vscode.download.prss.microsoft.com/dbazure/download/stable/x/code.deb"}, "host-spawn", false},
		{"no such name", vscode, Spec{Name: "nope.deb", URL: "https://example.com/x"}, "", false},
		// 2. a single source, wherever the new URL is
		{"single source", cellar, Spec{URL: "https://mirror.example.org/cellar-x86_64"}, "cellar-x86_64", true},
		// 3. the closest URL
		{"vendor's host", vscode, Spec{URL: "https://vscode.download.prss.microsoft.com/dbazure/download/stable/0f0d87fa9e96c856c5212fc86db137ac0d783365/code_1.148.0-1795000000_amd64.deb"}, "code.deb", true},
		{"github project among github projects", heidisql, Spec{URL: "https://github.com/HeidiSQL/HeidiSQL/releases/download/v12.23/build-qt6-v12.23.tgz"}, "heidisql.tgz", true},
		{"github owner/repo is case-insensitive", heidisql, Spec{URL: "https://github.com/heidisql/heidisql/releases/download/v12.23/build-qt6-v12.23.tgz"}, "heidisql.tgz", true},
		{"vendor beside prebuilt and a JRE", ibkr, Spec{URL: "https://download2.interactivebrokers.com/installers/ntws/10.40/ntws-10.40-standalone-linux-x64.sh"}, "ntws-installer.sh", true},
		{"the deeper directory wins", stirling, Spec{URL: "https://deb.debian.org/debian/pool/main/libi/libidn/libidn12_1.45-1_amd64.deb"}, "libidn12.deb", true},
		{"a github project alone among prebuilt", stirling, Spec{URL: "https://github.com/Stirling-Tools/Stirling-PDF/releases/download/v3.5.0/Stirling-PDF-linux-x86_64.deb"}, "stirling-pdf.deb", true},
		// 4. ambiguous
		{"two files of one github project", audiocpp, Spec{URL: "https://github.com/0xShug0/audio.cpp/releases/download/v0.9.6/audio-v0.9.6-bin-ubuntu-x64-cuda12.8-colab.tar.gz"}, "", false},
		{"three prebuilt packages", stirling, Spec{URL: "https://github.com/flatpark/prebuilt/releases/download/tesseract-v3/tesseract-tesseract-v3-gnome-51-x86_64.tar.xz"}, "", false},
		{"two Debian packages, no directory in common beyond the pool", stirling, Spec{URL: "https://deb.debian.org/debian/pool/main/x/xz/liblzma5.deb"}, "", false},
		{"another host", vscode, Spec{URL: "https://update.code.visualstudio.com/latest/linux-deb-x64/stable"}, "", false},
		{"another github project", heidisql, Spec{URL: "https://github.com/someone/fork/releases/download/v1/build.tgz"}, "", false},
		{"no sources", nil, Spec{URL: "https://example.com/x"}, "", false},
	} {
		got, auto, err := Choose(c.sources, c.spec)
		if c.want == "" {
			var ce *ChoiceError
			if !errors.As(err, &ce) {
				t.Errorf("%s: got %s, %v; want a ChoiceError", c.desc, got.Name, err)
				continue
			}
			// The error lists every source, for NAME=URL.
			for _, s := range c.sources {
				if !strings.Contains(err.Error(), "\n  "+s.Name+"  "+s.URI) {
					t.Errorf("%s: the error does not list %s:\n%v", c.desc, s.Name, err)
				}
			}
			continue
		}
		if err != nil || got.Name != c.want || auto != c.auto {
			t.Errorf("%s: got %q auto=%v, %v; want %q auto=%v", c.desc, got.Name, auto, err, c.want, c.auto)
		}
	}
}

const meta = `[Application]
name=org.example.App
runtime=org.freedesktop.Platform/x86_64/25.08
command=app

# the app's own package, then a helper
[Extra Data]
name=app.deb
checksum=` + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" + `
size=100
installed-size=1000
uri=https://dl.example.com/pool/app_1.0_amd64.deb
name1=helper.tar.xz
checksum1=` + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" + `
size1=  200
uri1=https://github.com/flatpark/prebuilt/releases/download/helper-v1/helper.tar.xz

[Extension org.example.App.Locale]
directory=share/runtime/locale
`

func TestRewrite(t *testing.T) {
	rep := Replacement{Name: "app.deb", File: File{URL: "https://dl.example.com/pool/app_1.1_amd64.deb", SHA256: sumN, Size: 123},
		Base: File{URL: "https://dl.example.com/pool/app_1.0_amd64.deb", SHA256: sumA, Size: 100}}
	got, err := Rewrite(meta, "app/org.example.App/x86_64/stable", []Replacement{rep})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.NewReplacer(
		"checksum="+sumA, "checksum="+sumN,
		"size=100\n", "size=123\n",
		"uri=https://dl.example.com/pool/app_1.0_amd64.deb", "uri=https://dl.example.com/pool/app_1.1_amd64.deb",
	).Replace(meta)
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	// flatpak reads what was meant: the new file, the helper unchanged.
	sources, _ := metadata.Parse(got, "ref")
	if len(sources) != 2 || sources[0].URI != rep.URL || sources[0].Checksum != sumN || sources[0].Size != 123 || sources[0].Name != "app.deb" ||
		sources[1].Checksum != sumB || sources[1].Size != 200 {
		t.Errorf("parsed back: %+v", sources)
	}
	// The second entry, whose size has blanks before it, keeps them.
	rep2 := Replacement{Name: "helper.tar.xz", File: File{URL: "https://example.org/h.tar.xz", SHA256: sumC, Size: 7}}
	got, err = Rewrite(meta, "ref", []Replacement{rep, rep2})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "\nsize1=  7\nuri1=https://example.org/h.tar.xz\n") || !strings.Contains(got, "checksum1="+sumC+"\n") || !strings.Contains(got, "\nsize=123\n") {
		t.Errorf("both:\n%s", got)
	}
	// Back to upstream's files: the original, byte for byte.
	back := []Replacement{{Name: "app.deb", File: rep.Base}, {Name: "helper.tar.xz", File: File{URL: "https://github.com/flatpark/prebuilt/releases/download/helper-v1/helper.tar.xz", SHA256: sumB, Size: 200}}}
	if again, err := Rewrite(got, "ref", back); err != nil || again != meta {
		t.Errorf("rewriting back: %v\n%s", err, again)
	}
	if _, err := Rewrite(meta, "ref", []Replacement{{Name: "other.deb"}}); err == nil {
		t.Errorf("no error for a source that is not there")
	}
}

// TestRewriteKeepsName: an entry named by its uri gets the name it had,
// which apply_extra expects, as a key of its own; CRLF and a last line
// without an end of line are kept.
func TestRewriteKeepsName(t *testing.T) {
	text := "[Application]\r\nruntime=a/b/c\r\n[Extra Data]\r\nchecksum=" + sumA + "\r\nsize=5\r\nuri=https://example.com/d/pkg_1.0.deb"
	rep := Replacement{Name: "pkg_1.0.deb", File: File{URL: "https://example.com/d/pkg_2.0.deb", SHA256: sumN, Size: 6}}
	got, err := Rewrite(text, "ref", []Replacement{rep})
	if err != nil {
		t.Fatal(err)
	}
	want := "[Application]\r\nruntime=a/b/c\r\n[Extra Data]\r\nchecksum=" + sumN + "\r\nsize=6\r\nuri=https://example.com/d/pkg_2.0.deb\nname=pkg_1.0.deb\n"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	sources, _ := metadata.Parse(got, "ref")
	if len(sources) != 1 || sources[0].Name != "pkg_1.0.deb" || sources[0].URI != rep.URL {
		t.Errorf("parsed back: %+v", sources)
	}
	// A URL GKeyFile has to escape reads back as it was.
	rep.URL = `https://example.com/a\b.deb`
	got, _ = Rewrite(text, "ref", []Replacement{rep})
	if sources, _ := metadata.Parse(got, "ref"); len(sources) != 1 || sources[0].URI != rep.URL {
		t.Errorf("escaped: %q -> %+v", got, sources)
	}
}

func TestCarry(t *testing.T) {
	reps := []Replacement{
		{Name: "app.deb", File: File{URL: "https://dl.example.com/pool/app_1.1_amd64.deb", SHA256: sumN, Size: 123},
			Base: File{URL: "https://dl.example.com/pool/app_1.0_amd64.deb", SHA256: sumA, Size: 100}},
		{Name: "helper.tar.xz", File: File{URL: "https://example.org/h.tar.xz", SHA256: sumC, Size: 7},
			Base: File{URL: "https://github.com/flatpark/prebuilt/releases/download/helper-v1/helper.tar.xz", SHA256: sumB, Size: 200}},
	}
	// Upstream rebuilt for something else: both kept.
	kept, dropped := Carry(reps, meta, "ref")
	if !reflect.DeepEqual(kept, reps) || len(dropped) != 0 {
		t.Errorf("unchanged upstream: kept %+v, dropped %+v", kept, dropped)
	}
	// Upstream moved to app 1.2: that patch goes, the helper's stays.
	moved := strings.Replace(meta, "checksum="+sumA, "checksum="+sumC, 1)
	kept, dropped = Carry(reps, moved, "ref")
	if len(kept) != 1 || kept[0].Name != "helper.tar.xz" || len(dropped) != 1 || dropped[0].Name != "app.deb" {
		t.Errorf("upstream changed app.deb: kept %+v, dropped %+v", kept, dropped)
	}
	// The same file at another address upstream: kept, the base follows.
	mirrored := strings.Replace(meta, "https://dl.example.com/pool/app_1.0_amd64.deb", "https://mirror.example.com/app_1.0_amd64.deb", 1)
	kept, _ = Carry(reps, mirrored, "ref")
	if len(kept) != 2 || kept[0].Base.URL != "https://mirror.example.com/app_1.0_amd64.deb" || kept[0].URL != reps[0].URL {
		t.Errorf("mirrored: %+v", kept)
	}
	// No longer there at all.
	kept, dropped = Carry(reps, "[Application]\nruntime=a/b/c\n", "ref")
	if len(kept) != 0 || len(dropped) != 2 {
		t.Errorf("gone: kept %+v, dropped %+v", kept, dropped)
	}
}

func TestMergeAndMarks(t *testing.T) {
	a := Replacement{Name: "b.deb", File: File{SHA256: sumA}}
	b := Replacement{Name: "a.deb", File: File{SHA256: sumB}}
	a2 := Replacement{Name: "b.deb", File: File{SHA256: sumC}}
	got := Merge([]Replacement{a, b}, []Replacement{a2})
	if len(got) != 2 || got[0] != b || got[1] != a2 {
		t.Errorf("Merge: %+v", got)
	}
	if m := Marks(got); !reflect.DeepEqual(m, []string{"a.deb:" + sumB, "b.deb:" + sumC}) {
		t.Errorf("Marks: %v", m)
	}
}
