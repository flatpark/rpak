package summary

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The files in testdata/repo are flatpak's own, see testdata/gen.sh;
// flatpark-summary.idx is dl.flatpark.org's of 2026-11-16.

func read(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func subsummary(t *testing.T) []byte {
	t.Helper()
	subs, err := Subsummaries(read(t, "repo/summary.idx"))
	if err != nil {
		t.Fatal(err)
	}
	digest, ok := subs[SubsummaryName("x86_64", "")]
	if len(subs) != 1 || !ok {
		t.Fatalf("subsummaries %v", subs)
	}
	zr, err := gzip.NewReader(bytes.NewReader(read(t, "repo/summaries/"+digest+".gz")))
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(data, digest); err != nil {
		t.Fatal(err)
	}
	if err := Verify(append(data, 0), digest); err == nil {
		t.Error("Verify accepted other data")
	}
	return data
}

func TestFind(t *testing.T) {
	for name, data := range map[string][]byte{
		"subsummary (version 1, xa.data)": subsummary(t),
		"compat summary (xa.cache)":       read(t, "repo/summary"),
	} {
		t.Run(name, func(t *testing.T) {
			refs, err := Find(data, "app/org.example.Extra/x86_64/")
			if err != nil {
				t.Fatal(err)
			}
			if len(refs) != 2 || refs[0].Name != "app/org.example.Extra/x86_64/beta" || refs[1].Name != "app/org.example.Extra/x86_64/stable" {
				t.Fatalf("refs %+v", refs)
			}
			for _, r := range refs {
				if len(r.Commit) != 64 {
					t.Errorf("%s: commit %q", r.Name, r.Commit)
				}
			}
			if !strings.Contains(refs[1].Metadata, "[Extra Data]\nname=app.deb\nchecksum="+strings.Repeat("0", 63)+"1\nsize=1000\nuri=https://example.org/app-1.0.deb\n") {
				t.Errorf("stable metadata:\n%s", refs[1].Metadata)
			}
			if !strings.Contains(refs[0].Metadata, "size=2000") {
				t.Errorf("beta metadata:\n%s", refs[0].Metadata)
			}

			refs, err = Find(data, "app/org.example.Plain/x86_64/stable")
			if err != nil || len(refs) != 1 || !strings.HasPrefix(refs[0].Metadata, "[Application]\nname=org.example.Plain\n") {
				t.Errorf("Plain: %+v %v", refs, err)
			}
			// A prefix of an id is not the id ("app/org.example.Extra/"
			// does not match org.example.ExtraMore), but Find takes
			// prefixes as given: callers end them with "/".
			if refs, err := Find(data, "app/org.example.Missing/"); err != nil || len(refs) != 0 {
				t.Errorf("Missing: %+v %v", refs, err)
			}
		})
	}
	// The same commits either way.
	a, _ := Find(subsummary(t), "app/")
	b, _ := Find(read(t, "repo/summary"), "app/")
	if len(a) != 3 || len(b) != 3 {
		t.Fatalf("%d and %d refs", len(a), len(b))
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].Commit != b[i].Commit || a[i].Metadata != b[i].Metadata {
			t.Errorf("%s differs: %+v %+v", a[i].Name, a[i], b[i])
		}
	}
}

func TestRealIndex(t *testing.T) {
	subs, err := Subsummaries(read(t, "flatpark-summary.idx"))
	if err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"x86_64", "aarch64"} {
		if len(subs[arch]) != 64 {
			t.Errorf("%s: %q", arch, subs[arch])
		}
	}
}

// Summaries come from the network: whatever they hold, reading them may
// fail but must not panic. Every truncation and every byte changed.
func TestBroken(t *testing.T) {
	for _, data := range [][]byte{read(t, "repo/summary"), subsummary(t)} {
		for cut := range data {
			Find(data[:cut], "app/")
		}
		for i := range data {
			for _, b := range []byte{0x00, 0xff, data[i] ^ 0x01} {
				changed := bytes.Clone(data)
				changed[i] = b
				Find(changed, "app/")
			}
		}
	}
	index := read(t, "flatpark-summary.idx")
	for cut := range index {
		Subsummaries(index[:cut])
	}
	if _, err := Subsummaries([]byte("not a summary index")); err == nil {
		t.Error("garbage index accepted")
	}
	if _, err := Find(bytes.Repeat([]byte{0xff}, 64), "app/"); err == nil {
		t.Error("garbage summary accepted")
	}
}
