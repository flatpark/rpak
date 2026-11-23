package app

import (
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestPlanStatus(t *testing.T) {
	defer func(d, k time.Duration) { planStatusDelay, planStatusTick = d, k }(planStatusDelay, planStatusTick)
	planStatusDelay, planStatusTick = 0, 5*time.Millisecond

	var w syncBuffer
	s := newPlanStatus(&w, term{tty: true, redraw: true, cols: 80})
	s.watch("", true)
	s.watch("fedora", true)
	s.watch("flathub", true)
	s.watch("flathub", false)
	s.watch("", false)
	deadline := time.Now().Add(2 * time.Second)
	want := "rpak: checking remotes: fedora (0s)\n"
	for !strings.Contains(w.String(), want) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	s.close()
	s.close()
	out := w.String()
	if !strings.Contains(out, want) {
		t.Fatalf("output %q", out)
	}
	if !strings.HasSuffix(out, "\x1b[1A\r\x1b[J") {
		t.Errorf("line not erased at the end: %q", out)
	}
}

func TestPlanStatusQuickOrNoTerminalIsSilent(t *testing.T) {
	var w syncBuffer
	s := newPlanStatus(&w, term{tty: true, redraw: true, cols: 80})
	s.watch("fedora", true)
	s.close()
	s = newPlanStatus(&w, term{})
	s.watch("fedora", true)
	s.close()
	if w.String() != "" {
		t.Errorf("output %q", w.String())
	}
}
