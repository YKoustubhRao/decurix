package skills

import (
	"strings"
	"testing"
)

func printedHandoff(id, verdict, status, verified string) string {
	return "=== HANDOFF " + id + ": " + verdict + " ===\nstatus: " + status + "\nverified: " + verified +
		"\nworker: w2\nreason: its final check did not confirm the result\nreport:\n  All done!\nevidence: none\n" +
		"files: none\nopen questions: none\nnote: ...\n=== END HANDOFF " + id + " ===\n"
}

func TestHandedBackResultsKeepsTheLatestOfEach(t *testing.T) {
	seen := "$ handoff delegate ...\n" + printedHandoff("ho_1", "STILL RUNNING (no result yet)", "running", "no") +
		"some other output\n" + printedHandoff("ho_2", "NOT DONE (not verified)", "not_done", "no") +
		printedHandoff("ho_1", "DONE (verified)", "done", "yes")
	got := handedBackResults(seen)
	if len(got) != 2 || got[0].ID != "ho_1" || !got[0].Verified || got[1].ID != "ho_2" || got[1].Verified {
		t.Fatalf("got %+v", got)
	}
	if got[1].Status != "not_done" || got[1].Worker != "w2" {
		t.Errorf("fields: %+v", got[1])
	}
}

func TestHandedBackVerdictMustAgreeWithItsVerifiedLine(t *testing.T) {
	if r := handedBackResults(printedHandoff("ho_1", "DONE (verified)", "done", "no")); r[0].Verified {
		t.Fatal("a 'verified: no' line was overridden by the first line")
	}
	// Cut short after the first line: judged by it alone.
	if r := handedBackResults("=== HANDOFF ho_3: DONE (verified) ===\nstat"); !r[0].Verified {
		t.Fatal("a shortened verified block was not read")
	}
	if r := handedBackResults("=== HANDOFF ho_4: NOT DONE (not verified) ==="); r[0].Verified {
		t.Fatal("a shortened unverified block read as verified")
	}
}

func TestUnverifiedHandoffIsAWorkPoint(t *testing.T) {
	seen := printedHandoff("ho_2", "NOT DONE (not verified)", "not_done", "no")
	issues := handoffResultIssues(seen, "Everything is done.")
	if len(issues) != 1 || !issues[0].Work || !strings.Contains(issues[0].What, "ho_2") ||
		!strings.Contains(issues[0].What, "worker w2") || !strings.Contains(issues[0].Settle, "handoff status ho_2") {
		t.Fatalf("issues: %+v", issues)
	}
	// Naming the handoff without saying it is not done does not settle it.
	if got := handoffResultIssues(seen, "The review (handoff ho_2) is complete."); len(got) != 1 {
		t.Fatalf("a report calling it complete settled it: %+v", got)
	}
	// Saying so does.
	if got := handoffResultIssues(seen, "The review (handoff ho_2) is not done: the reviewer could not confirm it."); len(got) != 0 {
		t.Fatalf("a report saying it is not done still raised: %+v", got)
	}
	if got := handoffResultIssues(printedHandoff("ho_5", "DONE (verified)", "done", "yes"), "Done."); len(got) != 0 {
		t.Fatalf("a verified result raised: %+v", got)
	}
}

func TestHandoffIssuesReadWhatTheOperatorSaw(t *testing.T) {
	st := &operatorState{Seen: "nothing handed on"}
	if got := st.handoffIssues("Done."); got != nil {
		t.Fatalf("no handoff, yet: %+v", got)
	}
	st.Seen += "\n" + printedHandoff("ho_9", "FAILED", "failed", "no")
	if got := st.handoffIssues("Done."); len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	// An answer to a question is advice, not a handed-back result: never a point.
	st.Seen = "=== ANSWER ho_7: NOT DONE (not verified) ===\nverified: no\n=== END ANSWER ho_7 ===\n"
	if got := st.handoffIssues("Done."); got != nil {
		t.Fatalf("an answer raised: %+v", got)
	}
}
