package app

import (
	"bytes"
	"testing"

	"github.com/flatpark/rpak/internal/doctor"
)

func TestWriteReport(t *testing.T) {
	r := doctor.Report{Sections: []doctor.Section{
		{Title: "Basics", Items: []doctor.Item{
			{Text: "rpak v1"},
			{Text: "flatpak: not found"},
		}},
		{Title: "Installations", Items: []doctor.Item{
			{Head: true, Text: "User installation: ~/.local/share/flatpak"},
			{Text: "writable, 1.0 GB free"},
			{Text: "global override ~/.local/share/flatpak/overrides/global:", Block: "[Context]\n\nfilesystems=home;"},
		}},
	}}
	var b bytes.Buffer
	if err := writeReport(&b, r); err != nil {
		t.Fatal(err)
	}
	want := "Paste all of this into the issue, between lines of three backticks (```).\n" +
		"\n== Basics\n" +
		"  rpak v1\n" +
		"  flatpak: not found\n" +
		"\n== Installations\n" +
		"User installation: ~/.local/share/flatpak\n" +
		"  writable, 1.0 GB free\n" +
		"  global override ~/.local/share/flatpak/overrides/global:\n" +
		"      [Context]\n" +
		"\n" +
		"      filesystems=home;\n"
	if got := b.String(); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}
