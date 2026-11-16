package app

import (
	"bytes"
	"strings"
	"testing"

	"github.com/flatpark/rpak/internal/doctor"
)

func TestWriteReport(t *testing.T) {
	r := doctor.Report{Sections: []doctor.Section{
		{Title: "Basics", Items: []doctor.Item{
			{Level: doctor.Info, Text: "rpak v1", Context: "Basics"},
			{Level: doctor.Fail, Text: "flatpak not found", Hint: "install it", Context: "Basics"},
		}},
		{Title: "Installations", Items: []doctor.Item{
			{Head: true, Text: "User installation: ~/.local/share/flatpak"},
			{Level: doctor.OK, Text: "writable", Context: "User installation"},
			{Level: doctor.Warn, Text: "global override", Hint: "review it", Block: "[Context]\n\nfilesystems=home;", Context: "User installation"},
		}},
	}}
	var b bytes.Buffer
	writeReport(&b, false, r)
	want := "Paste all of this into the issue, between lines of three backticks (```).\n" +
		"\n== Basics\n" +
		"  rpak v1\n" +
		"✗ flatpak not found\n" +
		"\n== Installations\n" +
		"User installation: ~/.local/share/flatpak\n" +
		"✓ writable\n" +
		"! global override\n" +
		"      [Context]\n" +
		"\n" +
		"      filesystems=home;\n" +
		"\n== Problems\n" +
		"✗ Basics: flatpak not found\n" +
		"    → install it\n" +
		"! User installation: global override\n" +
		"    → review it\n"
	if got := b.String(); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}

	b.Reset()
	writeReport(&b, true, doctor.Report{})
	if got := b.String(); !strings.Contains(got, "\x1b[32m✓ \x1b[0mnone found") {
		t.Errorf("colour: %q", got)
	}
}
