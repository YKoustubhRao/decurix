package skills

import (
	"strings"
	"testing"
)

// Found live (2026-10-26, four runs over): a file name holding a digit, named in the command, was masked in the
// output as a value of the command's own, so the header its read opened with matched no offered document.
func TestAFileNameInTheCommandIsNotMaskedInTheOutput(t *testing.T) {
	cmd := `office read '/w/SSS-26-27-0199.pdf' && office read "/w/1191 Some Customer.docx" && echo 76700`
	out := "SSS-26-27-0199.pdf: PDF, 1 page\nInvoice SSS/26-27/0199 Total 75,600.00\n1191 Some Customer.docx: Word document\nTotal 8,674.00\n76700"
	got := withoutCommandValues(out, cmd)
	if !strings.Contains(got, "SSS-26-27-0199.pdf:") || !strings.Contains(got, "1191 Some Customer.docx:") {
		t.Fatalf("file names masked: %q", got)
	}
	if strings.Contains(got, "\n76700") {
		t.Fatalf("a value the command gave is still vouched for: %q", got)
	}
	st := &operatorState{ScreenDocs: []string{"SSS-26-27-0199.pdf", "1191 Some Customer.docx"}}
	st.noteEvidence(cmd, got)
	for _, gap := range st.coverageGaps() {
		if strings.Contains(gap, "never read") {
			t.Errorf("a file read is a gap: %s", gap)
		}
	}
}

// A script that runs the reader itself names no file in its command; the headers its output holds credit the reads.
func TestReadsAScriptMadeAreCreditedByTheirHeaders(t *testing.T) {
	st := &operatorState{ScreenDocs: []string{"SSS-26-27-0199.pdf", "SSS-26-27-0200.pdf"}}
	cmd := "python3 - <<'PY'\nimport subprocess\nfor p in sorted(paths): print(subprocess.check_output(['office','read',p], text=True))\nPY"
	out := "SSS-26-27-0199.pdf: PDF, 1 page\nTotal 75,600.00\n\nSSS-26-27-0200.pdf: PDF, 1 page\nTotal 21,240.00\n"
	st.noteEvidence(cmd, withoutCommandValues(out, cmd))
	var docs []string
	for _, u := range st.Units {
		if u.Kind == "document" {
			docs = append(docs, u.Source)
		}
	}
	if strings.Join(docs, "|") != "SSS-26-27-0199.pdf|SSS-26-27-0200.pdf" {
		t.Fatalf("documents = %v", docs)
	}
	for _, gap := range st.coverageGaps() {
		if strings.Contains(gap, "never read") {
			t.Errorf("a file read is a gap: %s", gap)
		}
	}
}
