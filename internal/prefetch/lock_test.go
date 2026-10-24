package prefetch

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDirLock(t *testing.T) {
	base := t.TempDir()
	// Nothing to lock yet, and nothing created.
	release, locked, err := LockShared(base, false)
	if err != nil || locked {
		t.Fatalf("locked %v, err %v", locked, err)
	}
	release()
	if _, err := os.Stat(filepath.Join(base, "extra-data")); err == nil {
		t.Fatal("extra-data created without create")
	}

	release, locked, err = LockShared(base, true)
	if err != nil || !locked {
		t.Fatalf("locked %v, err %v", locked, err)
	}
	// Two runs at once are fine,
	other, locked, err := LockShared(base, false)
	if err != nil || !locked {
		t.Fatalf("second shared lock: %v", err)
	}
	// but rpak clean is kept out until both are done.
	if _, err := TryLockExclusive(base); !errors.Is(err, ErrInUse) {
		t.Errorf("exclusive while shared: %v", err)
	}
	release()
	if _, err := TryLockExclusive(base); !errors.Is(err, ErrInUse) {
		t.Errorf("exclusive while shared: %v", err)
	}
	other()
	exclusive, err := TryLockExclusive(base)
	if err != nil {
		t.Fatalf("exclusive after release: %v", err)
	}
	if _, err := TryLockExclusive(base); !errors.Is(err, ErrInUse) {
		t.Errorf("second exclusive: %v", err)
	}
	exclusive()
}
