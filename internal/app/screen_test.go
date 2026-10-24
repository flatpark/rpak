package app

import (
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// screen is a minimal terminal for the escape sequences the progress block
// uses: text, "\n", "\r", CSI n A (up), CSI J (erase below) and SGR, which
// it drops. Lines do not wrap; tests check widths themselves. It shows
// what is on the screen at any moment, and what is left at the end.
type screen struct {
	mu   sync.Mutex
	rows [][]rune
	row  int
	col  int
	raw  strings.Builder
}

func (s *screen) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.raw.Write(p)
	b := string(p)
	for len(b) > 0 {
		if strings.HasPrefix(b, "\x1b[") {
			end := strings.IndexFunc(b[2:], func(r rune) bool { return r >= '@' && r <= '~' })
			if end < 0 {
				break
			}
			param, final := b[2:2+end], b[2+end]
			b = b[3+end:]
			switch final {
			case 'A':
				n, err := strconv.Atoi(param)
				if err != nil {
					n = 1
				}
				s.row = max(s.row-n, 0)
			case 'J':
				s.grow()
				s.rows[s.row] = s.rows[s.row][:min(s.col, len(s.rows[s.row]))]
				s.rows = s.rows[:s.row+1]
			}
			continue
		}
		r, n := utf8.DecodeRuneInString(b)
		b = b[n:]
		switch r {
		case '\n':
			s.row++
			s.col = 0
		case '\r':
			s.col = 0
		default:
			s.grow()
			line := s.rows[s.row]
			for len(line) < s.col {
				line = append(line, ' ')
			}
			if s.col < len(line) {
				line[s.col] = r
			} else {
				line = append(line, r)
			}
			s.rows[s.row] = line
			s.col++
		}
	}
	return len(p), nil
}

func (s *screen) grow() {
	for len(s.rows) <= s.row {
		s.rows = append(s.rows, nil)
	}
}

// text is the screen's content, a line per row, without trailing empty rows.
func (s *screen) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var lines []string
	for _, r := range s.rows {
		lines = append(lines, string(r))
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}
