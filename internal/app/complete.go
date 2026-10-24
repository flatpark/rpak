package app

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"github.com/flatpark/rpak/internal/installation"
)

// `rpak complete LINE POINT CUR` speaks flatpak's completion protocol
// (app/flatpak-complete.c, app/flatpak-main.c): one candidate per line,
// the __FLATPAK_* markers for files and directories, and no space after an
// option ending in "=". rpak completes its own commands; everything else is
// flatpak's, so rpak asks flatpak and passes its answer on.

// completion is a command line being completed, as flatpak_completion_new()
// takes it apart.
type completion struct {
	line string
	// point is the cursor as a byte offset into line.
	point int
	// cur is the word around the cursor, unquoted as typed; shellCur is
	// what the shell considers the current word (CUR).
	cur, shellCur string
	// words are the words before cur, parsed with shell rules.
	words []shellWord
}

// newCompletion is flatpak_completion_new(), except that POINT counts
// characters, as bash's COMP_POINT and the zsh and fish scripts do, and is
// clamped to the line. It returns false where flatpak completes nothing:
// a bad POINT, nothing before the current word, or a line that cannot be
// parsed (an open quote).
func newCompletion(line, point, shellCur string) (*completion, bool) {
	n, err := strconv.Atoi(point)
	if err != nil {
		return nil, false
	}
	c := &completion{line: line, point: runeOffset(line, n), shellCur: shellCur}
	var begin int
	c.cur, begin = pickWordAt(line, c.point)
	if begin <= 0 {
		return nil, false
	}
	c.words, err = shellSplit(line[:begin])
	if err != nil {
		return nil, false
	}
	return c, true
}

// runeOffset is the byte offset of the n-th character of s, clamped to s.
// Invalid UTF-8 counts one character per byte.
func runeOffset(s string, n int) int {
	if n <= 0 {
		return 0
	}
	for i := range s {
		if n == 0 {
			return i
		}
		n--
	}
	return len(s)
}

// isWordSeparator is g_ascii_isspace().
func isWordSeparator(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\v' || c == '\f' || c == '\r'
}

// pickWordAt is pick_word_at(): the word around cursor, which extends past
// the cursor to the end of the word, and where it begins (-1 for an empty
// line).
func pickWordAt(s string, cursor int) (string, int) {
	if s == "" {
		return "", -1
	}
	if cursor < len(s) && isWordSeparator(s[cursor]) && (cursor == 0 || isWordSeparator(s[cursor-1])) {
		return "", cursor
	}
	for cursor > 0 && !isWordSeparator(s[cursor-1]) {
		cursor--
	}
	end := cursor
	for end < len(s) && !isWordSeparator(s[end]) {
		end++
	}
	return s[cursor:end], cursor
}

// shellWord is a word of a command line and where it is in the line.
type shellWord struct {
	text       string
	start, end int
}

// shellSplit is g_shell_parse_argv(): words separated by blanks, with
// single and double quotes, backslashes and # comments.
func shellSplit(s string) ([]shellWord, error) {
	var words []shellWord
	i := 0
	for i < len(s) {
		switch s[i] {
		case ' ', '\t', '\n':
			i++
			continue
		case '#':
			for i < len(s) && s[i] != '\n' {
				i++
			}
			continue
		}
		start := i
		var text strings.Builder
		for i < len(s) && s[i] != ' ' && s[i] != '\t' && s[i] != '\n' {
			switch c := s[i]; c {
			case '\'':
				end := strings.IndexByte(s[i+1:], '\'')
				if end < 0 {
					return nil, errors.New("unmatched quote")
				}
				text.WriteString(s[i+1 : i+1+end])
				i += end + 2
			case '"':
				i++
				for {
					if i >= len(s) {
						return nil, errors.New("unmatched quote")
					}
					if s[i] == '"' {
						i++
						break
					}
					if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("\"\\`$\n", s[i+1]) >= 0 {
						if s[i+1] != '\n' {
							text.WriteByte(s[i+1])
						}
						i += 2
						continue
					}
					text.WriteByte(s[i])
					i++
				}
			case '\\':
				if i+1 >= len(s) {
					return nil, errors.New("text ended just after a backslash")
				}
				if s[i+1] != '\n' {
					text.WriteByte(s[i+1])
				}
				i += 2
			default:
				text.WriteByte(c)
				i++
			}
		}
		words = append(words, shellWord{text.String(), start, i})
	}
	if len(words) == 0 {
		return nil, errors.New("empty command line")
	}
	return words, nil
}

// completeWord is flatpak_complete_word(): if candidate starts with cur, the
// part of it the shell should get, which starts where the shell's current
// word starts, but never before an "=" (bash splits words there).
func (c *completion) completeWord(candidate string) (string, bool) {
	if !strings.HasPrefix(candidate, c.cur) {
		return "", false
	}
	rest := len(c.cur)
	shellEnd := len(c.shellCur)
	for shellEnd > 0 && rest > 0 && c.shellCur[shellEnd-1] == candidate[rest-1] && c.shellCur[shellEnd-1] != '=' {
		rest--
		shellEnd--
	}
	return candidate[rest:], true
}

// flatpakArgs is what flatpak would get from the shell if the user had
// typed flatpak instead of rpak: the line with the first word replaced,
// and the cursor in bytes, which is what flatpak counts.
func (c *completion) flatpakArgs() []string {
	first := c.words[0]
	line := c.line[:first.start] + "flatpak" + c.line[first.end:]
	point := c.point - (first.end - first.start) + len("flatpak")
	return []string{"complete", line, strconv.Itoa(point), c.shellCur}
}

// completeCommand is `rpak complete LINE POINT [CUR]`. It changes nothing
// and prints no errors: a shell is waiting for candidates on stdout.
func completeCommand(argv []string, stdout io.Writer) int {
	shellCur := ""
	if len(argv) > 2 {
		shellCur = argv[2]
	}
	c, ok := newCompletion(argv[0], argv[1], shellCur)
	if !ok {
		return 1
	}
	// The command is found the way run() finds it, so that what is
	// completed is what would run.
	i := 1
	for i < len(c.words) && leadingGlobal(c.words[i].text) {
		i++
	}
	if i == len(c.words) {
		return c.commands(stdout)
	}
	switch c.words[i].text {
	case "clean":
		c.clean(stdout, c.words[i+1:])
		return 0
	case "completion":
		c.completion(stdout, c.words[i+1:])
		return 0
	}
	return c.flatpak(stdout)
}

// flatpak forwards the completion to flatpak, with its output and exit
// code.
func (c *completion) flatpak(stdout io.Writer) int {
	cmd := exec.Command(flatpakPath(), c.flatpakArgs()...)
	cmd.Stdout = stdout
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal())
		}
		return exit.ExitCode()
	}
	return 1
}

// ownCommands are the commands rpak adds to flatpak's.
var ownCommands = []string{"clean", "completion"}

// commands completes the command name: flatpak's candidates (commands, or
// global options for "-"), and rpak's own commands. If flatpak cannot be
// asked, at least the commands rpak knows of.
func (c *completion) commands(stdout io.Writer) int {
	var out bytes.Buffer
	cmd := exec.Command(flatpakPath(), c.flatpakArgs()...)
	cmd.Stdout = &out
	names := ownCommands
	if cmd.Run() != nil {
		names = append([]string{"install", "update"}, names...)
	}
	seen := map[string]bool{}
	var lines []string
	add := func(line string) {
		if !seen[line] {
			seen[line] = true
			lines = append(lines, line)
		}
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if line != "" {
			add(line)
		}
	}
	for _, name := range names {
		if word, ok := c.completeWord(name + " "); ok {
			add(word)
		}
	}
	for _, line := range lines {
		fmt.Fprintln(stdout, line)
	}
	return 0
}

// cleanFlag is an option of rpak clean (parseCleanArgs).
type cleanFlag struct {
	long  string
	short byte
	// value is for --installation=NAME; such an option may be repeated.
	value bool
	// repeat is for options that count again when repeated (-vv).
	repeat bool
}

// cleanFlags are offered in the order flatpak_complete_clean() of the fork
// offers its options: flatpak's global options, clean's, then the
// installation options.
var cleanFlags = []cleanFlag{
	{long: "verbose", short: 'v', repeat: true},
	{long: "ostree-verbose"},
	{long: "help", short: 'h'},
	{long: "dry-run"},
	{long: "user", short: 'u'},
	{long: "system"},
	{long: "installation", value: true},
}

// clean completes the options of rpak clean, which takes no other
// arguments, and installation names after --installation.
func (c *completion) clean(stdout io.Writer, args []shellWord) {
	emit := func(candidate string) {
		if word, ok := c.completeWord(candidate); ok {
			fmt.Fprintln(stdout, word)
		}
	}
	if len(args) > 0 && args[len(args)-1].text == "--installation" {
		for _, id := range installationIDs() {
			emit(id + " ")
		}
		return
	}
	if !strings.HasPrefix(c.cur, "-") {
		return
	}
	for _, f := range cleanFlags {
		if f.value {
			prefix := "--" + f.long + "="
			if strings.HasPrefix(c.cur, prefix) {
				for _, id := range installationIDs() {
					emit(prefix + id + " ")
				}
			} else {
				emit(prefix)
			}
			continue
		}
		// Like flatpak, switches already on the line are not offered again.
		given := !f.repeat && c.given(f)
		if !given {
			emit("--" + f.long + " ")
		}
		if f.short != 0 && !given {
			emit("-" + string(f.short) + " ")
		}
	}
}

// given reports whether f is among the words before the cursor, long or
// short, alone or in a group of short options (-uv).
func (c *completion) given(f cleanFlag) bool {
	for _, w := range c.words[1:] {
		switch {
		case w.text == "--"+f.long:
			return true
		case f.short != 0 && len(w.text) > 1 && w.text[0] == '-' && w.text[1] != '-' && strings.IndexByte(w.text[1:], f.short) >= 0:
			return true
		}
	}
	return false
}

// installationIDs are the ids --installation takes, as flatpak completes
// them (flatpak_get_system_installations()): the configured system
// installations and the default one, highest priority first, the default
// one after the others of priority 0.
func installationIDs() []string {
	configured, _ := installation.Env{}.Configured()
	var ids []string
	added := false
	for _, inst := range configured {
		if !added && inst.Priority < 0 {
			ids = append(ids, installation.DefaultID)
			added = true
		}
		ids = append(ids, inst.ID)
	}
	if !added {
		ids = append(ids, installation.DefaultID)
	}
	return ids
}

// completion completes rpak completion's only argument, the shell.
func (c *completion) completion(stdout io.Writer, args []shellWord) {
	var candidates []string
	switch {
	case strings.HasPrefix(c.cur, "-"):
		candidates = []string{"--help "}
	case len(args) == 0:
		candidates = []string{"bash ", "zsh ", "fish "}
	}
	for _, candidate := range candidates {
		if word, ok := c.completeWord(candidate); ok {
			fmt.Fprintln(stdout, word)
		}
	}
}
