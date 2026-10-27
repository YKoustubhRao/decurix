package skills

import (
	"strings"
	"testing"
)

func TestTheTraceKeepsAnOutputBoundedNotItsFirstLineAlone(t *testing.T) {
	short := shellResult{Output: "a.pdf\nb.pdf\nc.pdf"}
	if got := traceLine(short); got != "exit 0 · a.pdf\nb.pdf\nc.pdf" {
		t.Errorf("short = %q", got)
	}
	long := shellResult{Output: strings.Repeat("line of output text\n", 200)}
	got := traceLine(long)
	if !strings.HasPrefix(got, "exit 0 · line of output") || !strings.Contains(got, "more lines)") || len(got) > maxTraceChars+60 {
		t.Errorf("long = %d chars: %q...", len(got), got[:60])
	}
	// A failed command keeps its end: that is where the reason is.
	failed := shellResult{ExitCode: 1, Output: strings.Repeat("Typed a value into a field.\n", 100) +
		"Not pressed: the form does not hold what record 3307 says - Amount 4 is 3,200.00, the document gives 3,600.00"}
	got = traceLine(failed)
	if !strings.HasPrefix(got, "exit 1 · Typed a value") || !strings.Contains(got, "lines cut") ||
		!strings.HasSuffix(got, "the document gives 3,600.00") || len(got) > maxTraceChars+80 {
		t.Errorf("failed = %d chars: %q...%q", len(got), got[:40], got[len(got)-40:])
	}
	if got := traceLine(shellResult{}); got != "exit 0 · (no output)" {
		t.Errorf("empty = %q", got)
	}
}

// Found live (2026-10-26): a photo bill was downloaded, read and dropped; the report never named it, no point was raised.
func TestAPictureReadMustBeNamedInTheReport(t *testing.T) {
	st := &operatorState{}
	st.noteEvidence(`office read '/w/IMG_20261001_0932.jpg'`, "IMG_20261001_0932.jpg: an image, shown below.")
	st.noteEvidence(`office read '/w/Invoice_451.pdf'`, "Invoice_451.pdf: PDF\nInvoice 451 Total 20,800.00")
	st.noteWritten("gui type e5 \"20800\"")
	gaps := strings.Join(st.coverageGaps(), "\n")
	if !strings.Contains(gaps, "IMG_20261001_0932.jpg (a picture that was read") {
		t.Fatalf("the picture is not a gap: %s", gaps)
	}
	if strings.Contains(gaps, "Invoice_451.pdf") {
		t.Errorf("a document whose figure was entered is a gap: %s", gaps)
	}
	issues := st.coverageIssues("Done. Entered invoices 451 to 458 in the SALES block.")
	if len(issues) != 1 || !strings.Contains(issues[0].What, "IMG_20261001_0932.jpg") {
		t.Fatalf("the unnamed picture is not a point: %+v", issues)
	}
	if got := st.coverageIssues("Done. IMG_20261001_0932.jpg is bill 453, a cash sale of 18,000 at 5%, entered as row 29."); len(got) != 0 {
		t.Errorf("a picture the report names with what was entered is still a point: %+v", got)
	}
	if got := st.coverageIssues("Done. The photo IMG_20261001_0932.jpg is not a bill of this client - left out."); len(got) != 0 {
		t.Errorf("a picture the report says is left out is still a point: %+v", got)
	}
}
