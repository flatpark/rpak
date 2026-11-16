package doctor

import (
	"errors"
	"testing"
)

func TestRedactor(t *testing.T) {
	r := NewRedactor("/home/alice", map[string]string{"/home/bob": "bob"}, []string{"alice", "bob", "root", ""})
	for _, c := range []struct{ in, want string }{
		{"/home/alice", "~"},
		{"/home/alice/.local/share/flatpak", "~/.local/share/flatpak"},
		{"XDG_DATA_DIRS=/home/alice/.local/share/flatpak/exports/share:/usr/share", "XDG_DATA_DIRS=~/.local/share/flatpak/exports/share:/usr/share"},
		{"file:///home/alice/repo", "file://~/repo"},
		{"(b'/home/alice/x.svg', 54)", "(b'~/x.svg', 54)"},
		// Only whole path components and words.
		{"/home/alice2/x", "/home/alice2/x"},
		// Not the home, but the user name is still a name.
		{"/srv/home/alice/x", "/srv/home/<user>/x"},
		{"/home/bob/.var/app", "~<user>/.var/app"},
		{"owned by alice, alice's files", "owned by <user>, <user>'s files"},
		{"malice alice_x org.alice.App alice-1", "malice alice_x org.alice.App alice-1"},
		{"alice@example.org", "<user>@example.org"},
		{"SUDO_USER=bob", "SUDO_USER=<user>"},
		// root is no one in particular.
		{"/root/x root", "/root/x root"},
		// Passwords in URLs anywhere.
		{"remote https://u:secret@dl.example.org/repo/", "remote https://***@dl.example.org/repo/"},
	} {
		if got := r.String(c.in); got != c.want {
			t.Errorf("String(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// The format is doctor's own words; only the arguments are redacted.
	u := NewRedactor("/home/user", nil, []string{"user"})
	if got := u.Sprintf("User installation (user): %s, %v", "/home/user/.local/share/flatpak", errors.New("open /home/user/x: user")); got != "User installation (user): ~/.local/share/flatpak, open ~/x: <user>" {
		t.Errorf("Sprintf = %q", got)
	}
}

func TestRedactURL(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"http://u:p@h:3128", "http://***@h:3128"},
		{"http://u:p@h:3128/", "http://***@h:3128/"},
		{"socks5h://user@proxy.example:1080", "socks5h://***@proxy.example:1080"},
		{"u:p@h:3128", "***@h:3128"},
		{"http://h:3128", "http://h:3128"},
		{"h:3128", "h:3128"},
		{"http://u:p%40x@h:3128", "http://***@h:3128"},
		{"", ""},
	} {
		if got := RedactURL(c.in); got != c.want {
			t.Errorf("RedactURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := RedactURLList("http://a:b@h:1, localhost"); got != "http://***@h:1,localhost" {
		t.Errorf("RedactURLList = %q", got)
	}
	if got := RedactURLList("localhost,.example.org"); got != "localhost,.example.org" {
		t.Errorf("RedactURLList = %q", got)
	}
}
