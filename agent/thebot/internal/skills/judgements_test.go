package skills

import (
	"strings"
	"testing"

	"thebot/internal/llm"
)

// GST suite (2026-10-22): a strong model's misses were judgement calls it never noticed it made.

func TestJudgementListIsReadUnderAnyMarker(t *testing.T) {
	reply := "DONE: Filed the return.\n\nJUDGEMENTS:\n" +
		"- Documents issued 451-458, 1 cancelled | backed by: \"455 CANCELLED\" (SALES register row 9)\n" +
		"- Rent under reverse charge kept out of the ITC sheet | not backed: the earlier months had no purchases\n" +
		"\nChecked on the screen: filed."
	items, listed := judgementList(reply)
	if !listed || len(items) != 2 || !strings.HasPrefix(items[1], "Rent under reverse charge") {
		t.Fatalf("items %q listed %v", items, listed)
	}
	if items, listed := judgementList("**Judgements:** none"); !listed || len(items) != 0 {
		t.Errorf("none: %q %v", items, listed)
	}
	if _, listed := judgementList("DONE: all filed"); listed {
		t.Error("a report with no list read as one")
	}
}

func TestOnlyACallWhoseQuoteWasSeenIsBacked(t *testing.T) {
	seen := "SALES register\n455 | 07-09-2026 | CANCELLED | 0.00\nThe request: File the September GSTR-1 from the workbook."
	items := []string{
		`Bill 455 counted as cancelled | backed by: "455 | 07-09-2026 | CANCELLED" (register)`,
		`Period September | backed by: "file the September GSTR-1" (request)`,
		`Rent kept out of ITC | backed by: "rent is never a purchase credit" (the lesson)`,
		`Round-off left at 0 | not backed: nothing says otherwise`,
		`HSN 9987 included`,
	}
	got := unbackedCalls(items, seen)
	if len(got) != 3 || !strings.HasPrefix(got[0], "Rent kept") || !strings.HasPrefix(got[1], "Round-off") || got[2] != "HSN 9987 included" {
		t.Errorf("unbacked: %q", got)
	}
	// A cell quoted as "O62='Karnataka'" is backed by office read's line, where the two stand apart (GST suite T5).
	line := `[SEPTEMBER]  A62=2026-09-21  K62==IF(O62="Karnataka",ROUND(I62*H62/200,2),0) -> 15254.24`
	if got := unbackedCalls([]string{`Place of supply Karnataka | backed by: "O62='Karnataka'" (SALES row 62)`}, line); len(got) != 0 {
		t.Errorf("a cell and its value standing apart were not found: %q", got)
	}
	if got := unbackedCalls([]string{`Place of supply Kerala | backed by: "O62='Kerala'"`}, line); len(got) != 1 {
		t.Error("a value that is not there was taken as backed")
	}
	// A quote earlier in the call does not pair with the backing's (found live, GST suite T1).
	if got := unbackedCalls([]string{`Used it as "my PC" storage | backed by: "the September GSTR-1 from the workbook" (request)`},
		seen); len(got) != 0 {
		t.Errorf("a quoted word in the call broke its backing: %q", got)
	}
	// A quote that runs two lines together or drops a word is still found by a stretch of its words.
	if !quoteSeen("the September GSTR-1 from the workbook, as always", " "+plainWords(seen)+" ") {
		t.Error("a stretch of five words was not found")
	}
}

func TestARoundIsDueOnlyForRealWorkAndOncePerStretchOfIt(t *testing.T) {
	st := &operatorState{Hosted: true, CommandsSucceeded: 3}
	if st.judgementsDue() {
		t.Error("a quick question has no judgement calls to list")
	}
	st.CommandsSucceeded = 9
	if !st.judgementsDue() {
		t.Fatal("not due after real work")
	}
	st.JudgementRounds, st.JudgedAt = 1, 9
	if st.judgementsDue() {
		t.Error("due again with nothing done since")
	}
	st.CommandsSucceeded = 12
	if !st.judgementsDue() {
		t.Error("not due after more work")
	}
	st.Hosted = false
	if st.judgementsDue() {
		t.Error("a local model is left as it was")
	}
}

func TestTheSourcesOfACallIncludeTheLessonsInTheFixedPart(t *testing.T) {
	st := &operatorState{Seen: "invoice 451", Request: "file it", PrefixLen: 2, Messages: []llm.Message{
		{Role: "system", Content: "Lesson: rent under reverse charge is never listed as a purchase credit."},
		{Role: "user", Content: "contract ... file it"},
		{Role: "assistant", Content: "COMMAND: office read x"},
	}}
	got := unbackedCalls([]string{`Rent kept out of ITC | backed by: "rent under reverse charge is never listed as a purchase credit"`},
		st.judgementSources())
	if len(got) != 0 {
		t.Errorf("a lesson in the system frame did not back the call: %q", got)
	}
}

func TestTheQuestionAndTheApprovalSayWhatTheActionRestsOn(t *testing.T) {
	q := knowledgeQuestion([]string{"Rent kept out of ITC | not backed: earlier months"},
		[]string{"Cancelled bills count in documents issued - from https://example.org/gstr1-help"},
		`on screen it would click button "FILE WITH EVC" (e1)`)
	for _, want := range []string{`Before I go on to click button "FILE WITH EVC"`, "Is each a source you trust",
		"https://example.org/gstr1-help", "nothing you gave me or taught me settles", "1. Rent kept out of ITC",
		"documentation or a link"} {
		if !strings.Contains(q, want) {
			t.Errorf("question lacks %q:\n%s", want, q)
		}
	}
	if a := judgementsForApproval([]string{"Bill 455 cancelled | backed by: \"CANCELLED\""}); !strings.Contains(a, "What this rests on") {
		t.Errorf("approval: %q", a)
	}
}

func hostedRun(t *testing.T, replies []string) (Result, *scriptedChat) {
	t.Helper()
	chat := &scriptedChat{replies: replies}
	rc := operatorContext(t, chat)
	rc.Brain = llm.NewBrain(llm.Capabilities{Brain: "azure", Hosted: true})
	res, err := NewOperator().Run(rc, Subtask{Step: "work out the month's figures", Type: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	return res, chat
}

func TestADoneIsAskedForItsJudgementCallsAndAnUnbackedOneGoesToThePerson(t *testing.T) {
	work := []string{"COMMAND:\necho register row 455 CANCELLED", "COMMAND:\necho two", "COMMAND:\necho three", "COMMAND:\necho four"}
	// The independent check of what the backed call's quote establishes answers through the same scripted model.
	list := "DONE:\nThe figures are worked out.\n\nJUDGEMENTS:\n- Bill 455 left out | backed by: \"register row 455 CANCELLED\" (register)\n" +
		"- Rent counted as a purchase credit | not backed: earlier months looked like it"
	supported := `{"items": [{"n": 1, "verdict": "yes", "why": "the register says so"}]}`
	res, chat := hostedRun(t, append(work, "DONE:\nThe figures are worked out.", list, supported, list, supported))
	if !strings.Contains(chat.calls[5][len(chat.calls[5])-1].Content, "JUDGEMENTS:") {
		t.Fatal("the first DONE was not asked for its judgement calls")
	}
	if !strings.Contains(chat.calls[7][len(chat.calls[7])-1].Content, "Not settled yet") {
		t.Fatal("the unbacked call was not first sent back to be settled")
	}
	if res.Meta["needsUserInput"] != true || !strings.Contains(res.Output, "1. Rent counted as a purchase credit") ||
		strings.Contains(res.Output, "Bill 455") {
		t.Fatalf("result = %q %v", res.Output, res.Meta["needsUserInput"])
	}
}

func TestADoneWhoseCallsAreAllBackedIsReported(t *testing.T) {
	work := []string{"COMMAND:\necho register row 455 CANCELLED", "COMMAND:\necho two", "COMMAND:\necho three", "COMMAND:\necho four"}
	res, _ := hostedRun(t, append(work, "DONE:\nDone.",
		"DONE:\nDone.\n\nJUDGEMENTS:\n- Bill 455 left out | backed by: \"register row 455 CANCELLED\" (register)",
		`{"items": [{"n": 1, "verdict": "yes"}]}`))
	if !res.Verified || !strings.Contains(res.Output, "JUDGEMENTS:") {
		t.Fatalf("result = %+v", res)
	}
}

func TestAnApprovalIsAskedWithTheCallsTheActionRestsOn(t *testing.T) {
	stubWorkerTools(t,
		"echo 'NEEDS_APPROVAL: Approval needed before I click button \"FILE WITH EVC\" (e35) in \"File GSTR-1\": it files something on your behalf. Nothing was clicked or sent.'\nexit 3\n",
		"",
		"echo NEEDS_APPROVAL: Approval needed before I click button \"FILE WITH EVC\" (e35) in \"File GSTR-1\": it files something on your behalf. Nothing was clicked or sent.\r\nexit /b 3\r\n")
	res, chat := hostedRun(t, []string{
		"COMMAND:\necho register row 455 CANCELLED", "COMMAND:\necho two", "COMMAND:\necho three", "COMMAND:\necho four",
		"COMMAND:\ngui click e35",
		"JUDGEMENTS:\n- Bill 455 counted as cancelled | backed by: \"register row 455 CANCELLED\" (register)",
		`{"items": [{"n": 1, "verdict": "yes"}]}`,
	})
	if !strings.Contains(chat.calls[5][len(chat.calls[5])-1].Content, "Before this is put to the person for approval") {
		t.Fatal("the stopped action was not first asked for what it rests on")
	}
	reason, _ := res.Meta["approvalReason"].(string)
	if res.Meta["needsApproval"] != true || res.Meta["action"] != "gui click e35" ||
		!strings.Contains(reason, "What this rests on") || !strings.Contains(reason, "Bill 455 counted as cancelled") {
		t.Fatalf("result = %+v", res.Meta)
	}
}
