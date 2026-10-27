package skills

import (
	"strings"
	"testing"
)

// The case matrix before a commit (docs/learning-loop.md, phase A).

func caseState() *operatorState {
	st := &operatorState{Hosted: true, Request: "file the month's return from the register; it is up to date",
		Listings: []string{"Screen 1280x720. Active window: \"File Return | Portal\"\n" + `row "Documents | 451 | 458 | 8 | 0" (e13)`}}
	st.addUnit("row", "register.xlsx", "row 27: 454 | 36,000.00\nrow 28: 456 | 8,000.00")
	st.addUnit("document", "Bill_455.pdf", "Bill No 455  Date 16/09  Cashew 40 KGS  32,800.00")
	return st
}

const fileAction = `click button "FILE" (e20) in "File Return | Portal": it files something on your behalf`

func TestARecordTheSourcesDisagreeAboutWithNothingThatSaysWhyIsAPoint(t *testing.T) {
	st := caseState()
	chat := &scriptedChat{replies: []string{`{"records": [
		{"key": "455", "sources": {"register.xlsx": "absent", "Bill_455.pdf": "present, a bill of 32,800.00"}, "outcome": "unclear",
		 "disagree": true, "explained_by": {"source": "", "quote": ""}},
		{"key": "454", "sources": {"register.xlsx": "present"}, "outcome": "in", "disagree": false}]}`}}
	issues := (&OperatorSkill{}).caseMatrix(weighRC(chat), st, fileAction)
	if len(issues) != 1 || !strings.Contains(issues[0].What, "disagree about 455") ||
		!strings.Contains(issues[0].What, "register.xlsx: absent") || issues[0].Settle != "recheck" {
		t.Fatalf("issues = %+v", issues)
	}
	sent := chat.calls[0][1].Content
	if !strings.Contains(sent, "Bill_455.pdf") || !strings.Contains(sent, "Documents | 451 | 458") {
		t.Errorf("the check was not given the sources and the screen: %s", sent)
	}
}

func TestAnExplanationCountsOnlyWhenASourceHoldsIt(t *testing.T) {
	reply := func(quote string) *scriptedChat {
		return &scriptedChat{replies: []string{`{"records": [{"key": "455", "sources": {"register.xlsx": "absent",
			"Bill_455.pdf": "present"}, "outcome": "out", "disagree": true, "explained_by": {"source": "x", "quote": "` + quote + `"}}]}`}}
	}
	st := caseState()
	st.addUnit("document", "messages", "Client: please treat bill 455 as cancelled, the order was withdrawn")
	if got := (&OperatorSkill{}).caseMatrix(weighRC(reply("treat bill 455 as cancelled")), st, fileAction); len(got) != 0 {
		t.Errorf("an explanation a source holds was not taken: %+v", got)
	}
	if got := (&OperatorSkill{}).caseMatrix(weighRC(reply("the bill was voided by the client")), st, fileAction); len(got) != 1 {
		t.Errorf("an explanation no source holds was taken: %+v", got)
	}
	if got := (&OperatorSkill{}).caseMatrix(weighRC(reply("cancelled")), st, fileAction); len(got) != 1 {
		t.Errorf("a one-word quote was taken as an explanation: %+v", got)
	}
	// The screen being committed never explains itself.
	st.addUnit("screen", "gui observe", `row "Documents | 451 | 458 | 8 | 0" (e13)`)
	if got := (&OperatorSkill{}).caseMatrix(weighRC(reply("Documents 451 458 8 0")), st, fileAction); len(got) != 1 {
		t.Errorf("the committed screen explained a disagreement: %+v", got)
	}
}

// A page of another window - a chat, a ledger - is a source; the window being committed never is.
func TestAnotherWindowsScreenExplainsTheCommittedOneDoesNot(t *testing.T) {
	reply := func(quote string) *scriptedChat {
		return &scriptedChat{replies: []string{`{"records": [{"key": "455", "sources": {"register.xlsx": "absent"}, "outcome": "out",
			"disagree": true, "explained_by": {"source": "chat", "quote": "` + quote + `"}}]}`}}
	}
	st := caseState()
	st.noteEvidence("gui observe", "Screen 1280x720. Active window: \"Client Chat - Browser\"\n"+
		`text "Please ignore bill 455, it was raised in error and cancelled" (e40)`)
	st.noteEvidence("gui observe", "Screen 1280x720. Active window: \"File Return | Portal\"\n"+
		`text "Bill 455 left out of this return as instructed" (e50)`)
	chat := reply("it was raised in error and cancelled")
	if got := (&OperatorSkill{}).caseMatrix(weighRC(chat), st, fileAction); len(got) != 0 {
		t.Errorf("another window's page did not explain: %+v", got)
	}
	sent := chat.calls[0][1].Content
	at, end := strings.Index(sent, "OTHER SCREENS THE WORK READ"), strings.Index(sent, "THE ACTION:")
	if at < 0 || end < at || !strings.Contains(sent[at:end], "Client Chat") || strings.Contains(sent[at:end], "File Return") {
		t.Errorf("the check's other screens: %s", sent)
	}
	if got := (&OperatorSkill{}).caseMatrix(weighRC(reply("left out of this return as instructed")), st, fileAction); len(got) != 1 {
		t.Errorf("the committed window explained itself: %+v", got)
	}
}

func TestThePersonsWordsSettleADisagreement(t *testing.T) {
	st := caseState()
	st.addUnit("person", "the person's reply", "455 was withdrawn by the client, leave it out of everything")
	chat := &scriptedChat{replies: []string{`{"records": [{"key": "455", "sources": {"register.xlsx": "absent", "Bill_455.pdf": "present"},
		"outcome": "out", "disagree": true, "explained_by": {"source": "the person", "quote": "455 was withdrawn by the client"}}]}`}}
	if got := (&OperatorSkill{}).caseMatrix(weighRC(chat), st, fileAction); len(got) != 0 {
		t.Errorf("the person's decision did not settle it: %+v", got)
	}
}

// Found live (2026-10-23): looks at a workbook and a scan before a filing pushed the portal's page out of the last
// three listings, and both checks before the filing checked nothing. Listings are kept per window, and at the gate the
// screen - the window being committed - is looked at when none of it was kept.
func TestTheChecksBeforeACommitAlwaysHaveTheActionsWindow(t *testing.T) {
	st := &operatorState{Hosted: true}
	st.noteListing("gui observe", "Screen 1280x720. Active window: \"File Return | Portal\"\nold")
	for _, w := range []string{"Book", "Scan", "Mail"} {
		st.noteListing("gui observe", "Screen 1280x720. Active window: \""+w+"\"\nx")
	}
	st.noteListing("gui observe", "Screen 1280x720. Active window: \"File Return | Portal\"\nnew")
	if len(st.Listings) != 4 || !strings.HasSuffix(st.actionListing(fileAction), "new") {
		t.Fatalf("listings %d, the action's window: %q", len(st.Listings), st.actionListing(fileAction))
	}

	stubWorkerTools(t, "echo 'Screen 1280x720. Active window: \"File Return | Portal\"'\necho 'row \"Documents | 451 | 458\" (e13)'\n", "", "")
	fresh := &operatorState{Hosted: true}
	fresh.noteListing("gui observe", "Screen 1280x720. Active window: \"Book\"\nx")
	(&OperatorSkill{}).lookBeforeCommit(operatorContext(t, &scriptedChat{}), fresh, fileAction)
	if !strings.Contains(fresh.actionListing(fileAction), "Documents | 451 | 458") {
		t.Errorf("the screen was not looked at before the commit: %v", fresh.Listings)
	}
}

func TestNoCaseWithoutAScreenOfTheActionsWindow(t *testing.T) {
	st := caseState()
	st.Listings = []string{"Screen 1280x720. Active window: \"Image Viewer\"\n" + `label "37.8 MB" (e2)`}
	chat := &scriptedChat{}
	if got := (&OperatorSkill{}).caseMatrix(weighRC(chat), st, fileAction); got != nil || len(chat.calls) != 0 {
		t.Errorf("a case was made of another window: %+v (%d calls)", got, len(chat.calls))
	}
	local := caseState()
	local.Hosted = false
	if got := (&OperatorSkill{}).caseMatrix(weighRC(chat), local, fileAction); got != nil || len(chat.calls) != 0 {
		t.Error("a local brain made a side call")
	}
}
