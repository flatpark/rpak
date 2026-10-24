package metadata

import (
	"strings"
	"testing"
)

var (
	sumA = strings.Repeat("a", 64)
	sumB = strings.Repeat("b", 64)
	sumC = strings.Repeat("c", 64)
)

func TestParseSingle(t *testing.T) {
	text := `[Application]
name=com.google.Chrome
runtime=org.freedesktop.Platform/x86_64/24.08

[Extra Data]
name=chrome.deb
checksum=` + sumA + `
size=144849192
uri=https://dl.google.com/linux/chrome/deb/pool/main/g/google-chrome-stable/google-chrome-stable_156.0.8078.47-1_amd64.deb

[Extension com.google.Chrome.Locale]
directory=share/runtime/locale
`
	got, skipped := Parse(text, "app/com.google.Chrome/x86_64/stable")
	if len(got) != 1 || len(skipped) != 0 {
		t.Fatalf("got %#v, skipped %#v", got, skipped)
	}
	want := Source{
		Ref:      "app/com.google.Chrome/x86_64/stable",
		Name:     "chrome.deb",
		Checksum: sumA,
		Size:     144849192,
		URI:      "https://dl.google.com/linux/chrome/deb/pool/main/g/google-chrome-stable/google-chrome-stable_156.0.8078.47-1_amd64.deb",
	}
	if got[0] != want {
		t.Fatalf("got %#v, want %#v", got[0], want)
	}
}

func TestParseMultipleWithSuffixes(t *testing.T) {
	text := `[Extra Data]
name=one.bin
checksum=` + sumA + `
size=10
installed-size=20
uri=https://example.com/one.bin
name1=two.bin
checksum1=` + sumB + `
size1=11
uri1=http://example.com/two.bin
checksum2=` + sumC + `
size2=12
uri2=https://example.com/dir/a%20file.bin
`
	got, skipped := Parse(text, "ref")
	if len(got) != 3 || len(skipped) != 0 {
		t.Fatalf("got %#v, skipped %#v", got, skipped)
	}
	for i, want := range []struct {
		name, sum string
		size      int64
	}{{"one.bin", sumA, 10}, {"two.bin", sumB, 11}, {"a file.bin", sumC, 12}} {
		if got[i].Name != want.name || got[i].Checksum != want.sum || got[i].Size != want.size {
			t.Errorf("source %d = %#v, want %v", i, got[i], want)
		}
	}
}

func TestParseNameFromBasename(t *testing.T) {
	text := "[Extra Data]\nchecksum=" + sumA + "\nsize=1\nuri=https://example.com/pool/pkg_1.0%2Bdfsg_amd64.deb\n"
	got, _ := Parse(text, "ref")
	if len(got) != 1 || got[0].Name != "pkg_1.0+dfsg_amd64.deb" {
		t.Fatalf("got %#v", got)
	}
}

func TestParseSkipsInvalidEntries(t *testing.T) {
	cases := map[string]string{
		"query without name":     "checksum=" + sumA + "\nsize=1\nuri=https://example.com/file.bin?token=x\n",
		"fragment without name":  "checksum=" + sumA + "\nsize=1\nuri=https://example.com/file.bin#x\n",
		"directory without name": "checksum=" + sumA + "\nsize=1\nuri=https://example.com/dir/\n",
		"no path without name":   "checksum=" + sumA + "\nsize=1\nuri=https://example.com\n",
		"file uri":               "name=f\nchecksum=" + sumA + "\nsize=1\nuri=file:///etc/passwd\n",
		"ftp uri":                "name=f\nchecksum=" + sumA + "\nsize=1\nuri=ftp://example.com/f\n",
		"uppercase checksum":     "name=f\nchecksum=" + strings.Repeat("A", 64) + "\nsize=1\nuri=https://example.com/f\n",
		"short checksum":         "name=f\nchecksum=abcdef\nsize=1\nuri=https://example.com/f\n",
		"missing checksum":       "name=f\nsize=1\nuri=https://example.com/f\n",
		"zero size":              "name=f\nchecksum=" + sumA + "\nsize=0\nuri=https://example.com/f\n",
		"negative size":          "name=f\nchecksum=" + sumA + "\nsize=-5\nuri=https://example.com/f\n",
		"missing size":           "name=f\nchecksum=" + sumA + "\nuri=https://example.com/f\n",
		"slash in name":          "name=../f\nchecksum=" + sumA + "\nsize=1\nuri=https://example.com/f\n",
		"dot-dot name":           "name=..\nchecksum=" + sumA + "\nsize=1\nuri=https://example.com/f\n",
		"empty name":             "name=\nchecksum=" + sumA + "\nsize=1\nuri=https://example.com/f\n",
	}
	for label, body := range cases {
		got, skipped := Parse("[Extra Data]\n"+body, "ref")
		if len(got) != 0 || len(skipped) != 1 {
			t.Errorf("%s: got %#v, skipped %#v", label, got, skipped)
		}
	}
}

func TestParseKeepsValidEntriesNextToInvalidOnes(t *testing.T) {
	text := `[Extra Data]
uri=https://example.com/a.bin?x=1
checksum=` + sumA + `
size=1
uri1=https://example.com/b.bin
checksum1=` + sumB + `
size1=2
`
	got, skipped := Parse(text, "ref")
	if len(got) != 1 || got[0].Name != "b.bin" || len(skipped) != 1 || skipped[0].Key != "uri" {
		t.Fatalf("got %#v, skipped %#v", got, skipped)
	}
}

func TestParseKeyFileDetails(t *testing.T) {
	// Values are unescaped like g_key_file_get_string(), other groups and
	// localized keys are ignored, and the last duplicate key wins.
	text := `[Extra Data]
# comment
name = my\sfile.bin
checksum=` + sumB + `
checksum=` + sumA + `
size=3
uri=https://example.com/x
[Other]
uri1=https://example.com/ignored
`
	got, skipped := Parse(text, "ref")
	if len(got) != 1 || len(skipped) != 0 {
		t.Fatalf("got %#v, skipped %#v", got, skipped)
	}
	if got[0].Name != "my file.bin" || got[0].Checksum != sumA {
		t.Fatalf("got %#v", got[0])
	}
}

func TestParseNoExtraData(t *testing.T) {
	got, skipped := Parse("[Application]\nname=org.example.App\n", "ref")
	if len(got) != 0 || len(skipped) != 0 {
		t.Fatalf("got %#v, skipped %#v", got, skipped)
	}
}
