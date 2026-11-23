package app

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// planStatus shows, while rpak plans, which remotes it is waiting for:
// planning runs flatpak queries whose output rpak keeps, and a slow remote
// (an OCI remote's index, say) would otherwise leave the terminal blank
// for minutes before flatpak starts. The line appears only after
// planStatusDelay, so that a quick plan shows nothing, only on a terminal
// with cursor movement, and is erased before anything else is written.
type planStatus struct {
	w     io.Writer
	term  term
	start time.Time
	now   func() time.Time

	mu      sync.Mutex
	running map[string]int // queries running, by remote ("" for none)
	drawn   int            // width of the line on screen, -1 for none
	stopped bool
	stop    chan struct{}
	done    chan struct{}
}

// planStatusDelay and planStatusTick are variables for the tests.
var (
	planStatusDelay = time.Second
	planStatusTick  = time.Second
)

// newPlanStatus starts the status line on w; stop it with close. It does
// nothing where the line cannot be erased.
func newPlanStatus(w io.Writer, t term) *planStatus {
	s := &planStatus{w: w, term: t, now: time.Now, running: make(map[string]int), drawn: -1,
		stop: make(chan struct{}), done: make(chan struct{})}
	s.start = s.now()
	if !t.redraw {
		close(s.done)
		return s
	}
	go s.loop()
	return s
}

func (s *planStatus) loop() {
	defer close(s.done)
	select {
	case <-time.After(planStatusDelay):
	case <-s.stop:
		return
	}
	tick := time.NewTicker(planStatusTick)
	defer tick.Stop()
	for {
		s.mu.Lock()
		if !s.stopped {
			s.drawLocked()
		}
		s.mu.Unlock()
		select {
		case <-tick.C:
		case <-s.stop:
			return
		}
	}
}

// watch is the plan.Watch of the plan being shown.
func (s *planStatus) watch(remote string, running bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if running {
		s.running[remote]++
	} else if s.running[remote]--; s.running[remote] <= 0 {
		delete(s.running, remote)
	}
}

// line is the status line: "rpak: checking remotes: fedora, flathub (12s)".
func (s *planStatus) lineLocked() line {
	var names []string
	for r := range s.running {
		if r != "" {
			names = append(names, r)
		}
	}
	sort.Strings(names)
	var l line
	l.add("rpak: checking remotes", "")
	if len(names) > 0 {
		l.add(": "+strings.Join(names, ", "), sgrBold)
	}
	l.add(" ("+waited(s.now().Sub(s.start))+")", sgrFaint)
	return l
}

// drawLocked replaces the line on screen. The cursor stays at the start
// of the line below it, as below the progress block.
func (s *planStatus) drawLocked() {
	var b strings.Builder
	s.eraseTo(&b)
	l := s.lineLocked().clip(s.term.width())
	b.WriteString(l.render(s.term.color))
	b.WriteByte('\n')
	s.drawn = l.width()
	io.WriteString(s.w, b.String())
}

func (s *planStatus) eraseTo(b *strings.Builder) {
	if s.drawn < 0 {
		return
	}
	cols, _ := s.term.size()
	up := max(1, (s.drawn+cols-1)/cols)
	b.WriteString(fmt.Sprintf("\x1b[%dA\r\x1b[J", up))
	s.drawn = -1
}

// close stops the line and erases it.
func (s *planStatus) close() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	close(s.stop)
	var b strings.Builder
	s.eraseTo(&b)
	if b.Len() > 0 {
		io.WriteString(s.w, b.String())
	}
	s.mu.Unlock()
	<-s.done
}

// waited formats the time spent so far in whole seconds: "12s", "2m05s".
func waited(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
	return elapsed(d)
}
