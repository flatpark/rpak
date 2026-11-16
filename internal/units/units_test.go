package units

import "testing"

func TestSize(t *testing.T) {
	for n, want := range map[int64]string{
		0: "0 bytes", 1: "1 byte", 999: "999 bytes", 1000: "1.0 kB", 650_000: "650.0 kB",
		999_949: "999.9 kB", 999_950: "1.0 MB", 412_300_000: "412.3 MB", 2_400_000_000: "2.4 GB",
		1_000_000_000_000_000_000: "1.0 EB",
	} {
		if got := Size(n); got != want {
			t.Errorf("Size(%d) = %q, want %q", n, got, want)
		}
	}
}
