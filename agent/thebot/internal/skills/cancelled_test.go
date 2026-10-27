package skills

import (
	"strings"
	"testing"
)

// Found live (2026-10-27): the chat said a bill was cancelled; the worker had read it and saved the bill anyway.
func TestARecordSavedThatAMessageCalledOffIsTheWorkersToUndo(t *testing.T) {
	st := &operatorState{
		RecordLedger: "3 record(s) declared in this task:\n" +
			"- ABC1306 (from /w/IMG-1.jpg): saved and read back\n" +
			"- ABC1309 (from /w/IMG-4.jpg): saved and read back\n" +
			"- ABC1310 (from /w/IMG-5.jpg): declared, not saved",
		Listings: []string{`e12 link "Ravi (Client) 1309 cancel aagide sir, customer returned the pump"`},
	}
	issues := st.cancelledSavedIssues("Done. Entered ABC-1306 and ABC-1309.")
	if len(issues) != 1 || !strings.Contains(issues[0].What, "record ABC1309 was saved") || !issues[0].Work {
		t.Fatalf("issues = %+v", issues)
	}
	if got := st.cancelledSavedIssues("Done. 1309 was cancelled by the client - its voucher is deleted."); len(got) != 0 {
		t.Errorf("the report says what it did, still a point: %+v", got)
	}
	st.Listings = []string{`e12 link "Ravi (Client) 1310 cancelled, do not enter"`}
	if got := st.cancelledSavedIssues("Done."); len(got) != 0 {
		t.Errorf("a record not saved is a point: %+v", got)
	}
}
