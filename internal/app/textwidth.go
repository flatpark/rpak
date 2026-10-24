package app

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// The terminal output measures text in cells, not runes: East Asian wide
// characters and most emoji take two cells, combining marks none. This is
// a small approximation of wcwidth(3) that never says narrow for a
// character a terminal draws wide, which is what keeps lines from wrapping.

// wideRanges are the East Asian Wide and Fullwidth ranges of Unicode 15,
// including the emoji that are presented wide by default, merged into
// coarser ranges where that only adds unassigned code points.
var wideRanges = [][2]rune{
	{0x1100, 0x115F}, {0x231A, 0x231B}, {0x2329, 0x232A}, {0x23E9, 0x23EC},
	{0x23F0, 0x23F0}, {0x23F3, 0x23F3}, {0x25FD, 0x25FE}, {0x2614, 0x2615},
	{0x2648, 0x2653}, {0x267F, 0x267F}, {0x2693, 0x2693}, {0x26A1, 0x26A1},
	{0x26AA, 0x26AB}, {0x26BD, 0x26BE}, {0x26C4, 0x26C5}, {0x26CE, 0x26CE},
	{0x26D4, 0x26D4}, {0x26EA, 0x26EA}, {0x26F2, 0x26F3}, {0x26F5, 0x26F5},
	{0x26FA, 0x26FA}, {0x26FD, 0x26FD}, {0x2705, 0x2705}, {0x270A, 0x270B},
	{0x2728, 0x2728}, {0x274C, 0x274C}, {0x274E, 0x274E}, {0x2753, 0x2755},
	{0x2757, 0x2757}, {0x2795, 0x2797}, {0x27B0, 0x27B0}, {0x27BF, 0x27BF},
	{0x2B1B, 0x2B1C}, {0x2B50, 0x2B50}, {0x2B55, 0x2B55},
	{0x2E80, 0x303E}, {0x3041, 0x33FF}, {0x3400, 0x4DBF}, {0x4E00, 0x9FFF},
	{0xA000, 0xA4CF}, {0xA960, 0xA97F}, {0xAC00, 0xD7A3}, {0xF900, 0xFAFF},
	{0xFE10, 0xFE19}, {0xFE30, 0xFE6F}, {0xFF00, 0xFF60}, {0xFFE0, 0xFFE6},
	{0x16FE0, 0x16FE4}, {0x17000, 0x18CFF}, {0x1AFF0, 0x1B2FF},
	{0x1F004, 0x1F004}, {0x1F0CF, 0x1F0CF}, {0x1F18E, 0x1F18E}, {0x1F191, 0x1F19A},
	{0x1F200, 0x1F202}, {0x1F210, 0x1F23B}, {0x1F240, 0x1F248}, {0x1F250, 0x1F251},
	{0x1F260, 0x1F265}, {0x1F300, 0x1F64F}, {0x1F680, 0x1F6FF}, {0x1F7E0, 0x1F7EB},
	{0x1F7F0, 0x1F7F0}, {0x1F90C, 0x1F9FF}, {0x1FA70, 0x1FAFF},
	{0x20000, 0x2FFFD}, {0x30000, 0x3FFFD},
}

// runeWidth is the number of cells r takes on its own.
func runeWidth(r rune) int {
	switch {
	case r == 0x200D, r >= 0xFE00 && r <= 0xFE0F, r >= 0xE0100 && r <= 0xE01EF:
		return 0 // zero width joiner, variation selectors
	case r < 0x300:
		return 1 // the common case; control characters are sanitized away
	case unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf):
		return 0
	}
	lo, hi := 0, len(wideRanges)
	for lo < hi {
		m := (lo + hi) / 2
		switch {
		case r < wideRanges[m][0]:
			hi = m
		case r > wideRanges[m][1]:
			lo = m + 1
		default:
			return 2
		}
	}
	return 1
}

// clusters splits s into characters with the zero-width runes that follow
// them, and their widths. A character followed by VARIATION SELECTOR-16
// (emoji presentation, as in "❤️") is counted as wide.
func clusters(s string, fn func(c string, width int) bool) {
	for len(s) > 0 {
		r, n := utf8.DecodeRuneInString(s)
		w := runeWidth(r)
		for n < len(s) {
			next, m := utf8.DecodeRuneInString(s[n:])
			if runeWidth(next) != 0 {
				break
			}
			if next == 0xFE0F && w == 1 {
				w = 2
			}
			n += m
		}
		if !fn(s[:n], w) {
			return
		}
		s = s[n:]
	}
}

// textWidth is the number of cells s takes.
func textWidth(s string) int {
	total := 0
	clusters(s, func(_ string, w int) bool { total += w; return true })
	return total
}

// headCells returns the longest prefix of s that fits in width cells.
func headCells(s string, width int) string {
	end, used := 0, 0
	clusters(s, func(c string, w int) bool {
		if used+w > width {
			return false
		}
		used += w
		end += len(c)
		return true
	})
	return s[:end]
}

// tailCells returns the longest suffix of s that fits in width cells.
func tailCells(s string, width int) string {
	var parts []string
	clusters(s, func(c string, _ int) bool { parts = append(parts, c); return true })
	start, used := len(s), 0
	for i := len(parts) - 1; i >= 0; i-- {
		w := textWidth(parts[i])
		if used+w > width {
			break
		}
		used += w
		start -= len(parts[i])
	}
	return s[start:]
}

const ellipsis = "…"

// truncateEnd shortens s to at most width cells, ending it with "…".
func truncateEnd(s string, width int) string {
	if textWidth(s) <= width {
		return s
	}
	if width < 1 {
		return ""
	}
	return headCells(s, width-1) + ellipsis
}

// truncateMiddle shortens s to at most width cells by replacing its middle
// with "…", keeping the start and the end, which for a file name is the
// extension and for an ID or a host the most specific part. At least a
// third of the room goes to the end, more if needed to keep a short
// extension whole.
func truncateMiddle(s string, width int) string {
	if textWidth(s) <= width {
		return s
	}
	if width < 5 {
		return truncateEnd(s, width)
	}
	room := width - 1
	tail := room / 3
	if i := strings.LastIndexByte(s, '.'); i > 0 {
		if ext := textWidth(s[i:]); ext > tail && ext <= room/2+1 {
			tail = ext
		}
	}
	end := tailCells(s, tail)
	return headCells(s, room-textWidth(end)) + ellipsis + end
}

// sanitize replaces control characters, which could move the cursor or
// change the terminal's state, by "?". File names, hosts and error
// messages come from remote metadata and servers.
func sanitize(s string) string {
	clean := true
	for _, r := range s {
		if r < 0x20 || r == 0x7F || (r >= 0x80 && r < 0xA0) || r == utf8.RuneError {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7F || (r >= 0x80 && r < 0xA0) || r == utf8.RuneError {
			r = '?'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// padRight pads s with spaces to width cells.
func padRight(s string, width int) string {
	if n := width - textWidth(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// padLeft right-aligns s in width cells.
func padLeft(s string, width int) string {
	if n := width - textWidth(s); n > 0 {
		return strings.Repeat(" ", n) + s
	}
	return s
}
