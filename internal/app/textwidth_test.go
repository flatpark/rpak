package app

import (
	"strings"
	"testing"
)

func TestTextWidth(t *testing.T) {
	for s, want := range map[string]int{
		"":                 0,
		"app.deb":          7,
		"微信输入法.deb":        14,
		"한글":               4,
		"ｆｕｌｌ":             8,
		"cafe\u0301":       4, // combining acute accent
		"fonts-✨.zip":      12,
		"\u2764\ufe0f":     2, // text-default heart with emoji presentation
		"❤":                1,
		"📦":                2,
		"a\u200db":         2,
		"…█▏":              3,
		"\U00020000":       2, // CJK extension B
		"zero\u200bwidth":  9,
		"x\ufe0f":          2, // VS16 after a narrow character
		"ascii only, 1234": 16,
	} {
		if got := textWidth(s); got != want {
			t.Errorf("textWidth(%q) = %d, want %d", s, got, want)
		}
	}
}

func TestTruncateMiddle(t *testing.T) {
	for _, c := range []struct {
		in    string
		width int
		want  string
	}{
		{"app.deb", 20, "app.deb"},
		{"app.deb", 7, "app.deb"},
		{"Vendor-Collaboration-Suite-2026.10-linux-x86_64.AppImage", 30, "Vendor-Collaboration….AppImage"},
		{"google-chrome-stable_131.0.6778.85-1_amd64.deb", 20, "google-chrome…64.deb"},
		{"微信输入法-专业版-安装包.deb", 20, "微信输入法-专…包.deb"},
		{"微信输入法-专业版-安装包.deb", 15, "微信输入法….deb"},
		// An odd width cannot always be filled with wide characters.
		{"微信输入法-专业版-安装包", 10, "微信输…包"},
		{"org.freedesktop.Platform.openh264//2.4.1", 24, "org.freedesktop.…//2.4.1"},
		{"abcdefghij", 4, "abc…"},
		{"abcdefghij", 1, "…"},
		{"abcdefghij", 0, ""},
	} {
		got := truncateMiddle(c.in, c.width)
		if got != c.want {
			t.Errorf("truncateMiddle(%q, %d) = %q, want %q", c.in, c.width, got, c.want)
		}
		if w := textWidth(got); w > c.width {
			t.Errorf("truncateMiddle(%q, %d) is %d cells wide", c.in, c.width, w)
		}
	}
	// Every width, for names with wide characters and emoji.
	for _, s := range []string{"微信输入法-专业版-安装包.deb", "fonts-✨-emoji-pack-\u2764\ufe0f-final.tar.gz", "a中b文c字d.x"} {
		for w := 0; w <= textWidth(s)+1; w++ {
			got := truncateMiddle(s, w)
			if textWidth(got) > w || (w >= 2 && textWidth(got) < w-1) {
				t.Errorf("truncateMiddle(%q, %d) = %q, %d cells", s, w, got, textWidth(got))
			}
		}
	}
}

func TestTruncateEnd(t *testing.T) {
	if got := truncateEnd("HTTP 503 Service Unavailable", 10); got != "HTTP 503 …" {
		t.Errorf("got %q", got)
	}
	if got := truncateEnd("中文中文", 5); got != "中文…" {
		t.Errorf("got %q", got)
	}
}

func TestSanitize(t *testing.T) {
	if got := sanitize("evil\x1b[2J\x07name\n.deb\u009b"); got != "evil?[2J?name?.deb?" {
		t.Errorf("got %q", got)
	}
	if got := sanitize("微信.deb"); got != "微信.deb" {
		t.Errorf("got %q", got)
	}
	if got := sanitize("bad\xffutf8"); strings.ContainsRune(got, '\ufffd') || got != "bad?utf8" {
		t.Errorf("got %q", got)
	}
}
