package gvariant

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// TestDeployDataMatchesGLib replaces the runtime of every deploy fixture
// (testdata/gen-deploy.py) and compares with what GLib serialises; a
// fixture without runtime must come out unchanged.
func TestDeployDataMatchesGLib(t *testing.T) {
	paths, _ := filepath.Glob("testdata/*.forced.deploy")
	if len(paths) < 7 {
		t.Fatalf("only %d fixtures; run testdata/gen-deploy.sh", len(paths))
	}
	for _, p := range paths {
		name := strings.TrimSuffix(filepath.Base(p), ".forced.deploy")
		t.Run(name, func(t *testing.T) {
			in := read(t, name+".deploy")
			want := read(t, name+".forced.deploy")
			d, err := ParseDeployData(in)
			if err != nil {
				t.Fatal(err)
			}
			if out := d.Serialize(); !bytes.Equal(out, in) {
				t.Fatalf("round trip differs:\n got %x\nwant %x", head(out), head(in))
			}
			if len(d.Commit) != 64 || d.Origin == "" {
				t.Errorf("origin %q commit %q", d.Origin, d.Commit)
			}
			// The new value is the one in the GLib output.
			wd, err := ParseDeployData(want)
			if err != nil {
				t.Fatal(err)
			}
			newValue, hasRuntime := wd.String("runtime")
			if found := d.SetString("runtime", newValue); found != hasRuntime {
				t.Errorf("SetString found = %v", found)
			}
			if out := d.Serialize(); !bytes.Equal(out, want) {
				t.Errorf("got %d bytes, GLib %d\n got %x\nwant %x", len(out), len(want), head(out), head(want))
			}
			if got, _ := d.String("runtime"); hasRuntime && got != newValue {
				t.Errorf("runtime %q", got)
			}
		})
	}
}

func TestDeployDataReal(t *testing.T) {
	d, err := ParseDeployData(read(t, "real.deploy"))
	if err != nil {
		t.Fatal(err)
	}
	if rt, ok := d.String("runtime"); !ok || rt != "org.freedesktop.Platform/x86_64/25.08" {
		t.Errorf("runtime %q %v", rt, ok)
	}
	if d.Origin != "local" {
		t.Errorf("origin %q", d.Origin)
	}
}

func TestDeployDataRefusesGarbage(t *testing.T) {
	in := read(t, "middle.deploy")
	for _, bad := range [][]byte{nil, in[:len(in)-1], append(append([]byte{}, in...), 0)} {
		if _, err := ParseDeployData(bad); err == nil {
			t.Errorf("%d bytes accepted", len(bad))
		}
	}
}
