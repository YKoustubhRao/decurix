package skills

import (
	"strings"
	"testing"
)

// GST suite T8 (2026-10-22): five invoices saved, the phone photo among them never read; and a report that said the
// client's challan did not match the workbook, reported done.

func TestADocumentTheWorkSavedAndNeverReadIsFound(t *testing.T) {
	commands := []string{
		`cd '/home/headless/PC/UAT/06. SEPTEMBER' && curl -fsSLo 'Invoice_451.pdf' 'http://x/451.pdf' && curl -fsSLo 'IMG_20261001_0932.jpg' 'http://x/img'`,
		`cp ~/Downloads/PB_KCP-778.pdf '/home/headless/PC/UAT/06. SEPTEMBER/Purchases/'`,
		`office read '/home/headless/PC/UAT/06. SEPTEMBER/'*.pdf`,
		`office read '/home/headless/PC/UAT/06. SEPTEMBER/Purchases/PB_KCP-778.pdf'`,
	}
	got := unreadFetched(commands)
	if len(got) != 1 || got[0] != "IMG_20261001_0932.jpg" {
		t.Fatalf("unread = %v, want the photo only", got)
	}
	if n := unreadFetchedNudge(got); !strings.Contains(n, `"IMG_20261001_0932.jpg"`) || !strings.Contains(n, "read it") {
		t.Errorf("nudge: %q", n)
	}
	if got := unreadFetched(append(commands, `office read '/home/headless/PC/UAT/06. SEPTEMBER/IMG_20261001_0932.jpg'`)); len(got) != 0 {
		t.Errorf("read afterwards, still listed: %v", got)
	}
	if got := unreadFetched([]string{`grep -o 'x' a.pdf`, `wget -O report.html http://x`}); len(got) != 0 {
		t.Errorf("not documents fetched: %v", got)
	}
}

func TestAReportOfAMismatchIsSentBackOnce(t *testing.T) {
	for _, report := range []string{"The saved challan says 13,887.30, which does not match the workbook's 13,029.80.",
		"HSN summary (B2C) taxable value 0.00 doesn't match B2C (Others)", "There is a mismatch between the two."} {
		if !mismatchRe.MatchString(report) {
			t.Errorf("not caught: %q", report)
		}
	}
	if mismatchRe.MatchString("All figures match the workbook.") {
		t.Error("a match taken for a mismatch")
	}
}

func TestAQuestionAskingToApproveWhatTheGateAsksIsSentBack(t *testing.T) {
	for _, q := range []string{
		"Please approve the GSTR-3B filing click. I'm ready to click FILE WITH EVC.",
		"I've drafted the message and am ready to click Send. Please approve this final outbound send click.",
		"Please complete the OTP step on this worker's screen when I click FILE WITH EVC, then reply with the OTP.",
		"Please approve the payment step for GSTR-3B when it is put to you on screen; I will continue right after that.",
	} {
		if !selfApprovalRe.MatchString(q) {
			t.Errorf("not sent back: %q", q)
		}
	}
	for _, q := range []string{
		"Please give me the OTP the portal says it sent to the registered mobile.",
		"Which of the two customers named Asha should the invoice go to?",
		"I need the GST portal password for kavyacon.",
		"May I have your permission to delete the old logs?",
	} {
		if selfApprovalRe.MatchString(q) {
			t.Errorf("a real question sent back: %q", q)
		}
	}
}
