package codemap

import "testing"

// Each kind of file is summarised by its own header comment, cut to its first sentence; a file with none gets none.
func TestSummarizeReadsEachFilesOwnHeader(t *testing.T) {
	cases := []struct{ name, src, want, pkg string }{
		{"a.go", "// Package a does one thing. And more.\npackage a\n", "Package a does one thing.", "Package a does one thing."},
		{"b.go", "package b\n\nfunc helper() {}\n\n// Run starts the thing.\nfunc Run() {}\n", "Run starts the thing.", ""},
		{"c.py", "import os\n\n# ====\n# Checking a task was done\n#\n# More.\n", "Checking a task was done", ""},
		{"d.py", "\"\"\"Reads a file. Then more.\"\"\"\nimport os\n", "Reads a file.", ""},
		{"e.ps1", "<#\n.SYNOPSIS\n    Builds the app.\n.DESCRIPTION\n    Long.\n#>\n", "Builds the app.", ""},
		{"f.sh", "#!/bin/sh\n# Opens Decurix, e.g. on Linux. Then more.\nset -e\n", "Opens Decurix, e.g. on Linux.", ""},
		{"g.md", "Intro\n# Title here\n", "Title here", ""},
		{"h.json", "{}", "", ""},
		{"i.go", "package i\n\nfunc x() {}\n", "", ""},
		{"j.bin", "\x00\x01", "", ""},
	}
	for _, c := range cases {
		got, pkg := Summarize(c.name, []byte(c.src))
		if got != c.want || pkg != c.pkg {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", c.name, got, pkg, c.want, c.pkg)
		}
	}
}

// A summary stays on one line in a Markdown list: long ones are cut, and table pipes are escaped.
func TestSentenceIsOneSafeLine(t *testing.T) {
	long := ""
	for i := 0; i < 50; i++ {
		long += "word "
	}
	if r := []rune(sentence(long)); len(r) > maxSummary {
		t.Fatalf("summary of %d runes", len(r))
	}
	if got := sentence("a | b"); got != `a \| b` {
		t.Fatalf("got %q", got)
	}
}
