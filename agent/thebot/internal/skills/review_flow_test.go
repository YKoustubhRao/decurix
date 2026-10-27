package skills

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"thebot/internal/llm"
)

// The review's flow around approvals and the person's words (independent review of the harness, 2026-10-22).

const gatedGui = "echo 'NEEDS_APPROVAL: Approval needed before I click button \"FILE\" (e35): it files something on your behalf. Nothing was clicked or sent.'\nexit 3\n"

func TestAListWithACommandAtAnApprovalRunsTheCommandFirst(t *testing.T) {
	stubWorkerTools(t, gatedGui, "", "")
	_, chat := hostedRun(t, []string{
		"COMMAND:\necho register row 455 CANCELLED", "COMMAND:\necho two", "COMMAND:\necho three", "COMMAND:\necho four",
		"COMMAND:\ngui click e35",
		"JUDGEMENTS:\n- Bill 455 counted as cancelled | backed by: \"register row 455 CANCELLED\" (register)\n\nCOMMAND:\necho fixed-first",
		"DONE:\nStopped here.",
	})
	ran := false
	for _, call := range chat.calls {
		if strings.Contains(call[len(call)-1].Content, "fixed-first") {
			ran = true
		}
	}
	if !ran {
		t.Fatal("the command sent beside the list was dropped and the action put to the person instead")
	}
}

func TestAGateStoppingAPointBeingSettledCarriesThePointsToTheApproval(t *testing.T) {
	stubWorkerTools(t, gatedGui, "", "")
	res, _ := hostedRun(t, []string{
		"COMMAND:\necho register row 455 CANCELLED", "COMMAND:\necho two", "COMMAND:\necho three", "COMMAND:\necho four",
		"COMMAND:\ngui click e35",
		"JUDGEMENTS:\n- Rate taken as 18% | not backed: earlier months",
		`{"issues": [{"what": "Rate 18% is in no document read", "settle": "recheck"}]}`,
		"COMMAND:\ngui click e36", // settling, and stopped by the gate again
	})
	reason, _ := res.Meta["approvalReason"].(string)
	if res.Meta["needsApproval"] != true || !strings.Contains(reason, "Still open after an independent review") ||
		!strings.Contains(reason, "Rate 18% is in no document read") {
		t.Fatalf("result = %+v", res.Meta)
	}
}

// Found live (2026-10-23): asked first as a question and then for the approval, every gated click cost the person two.
// What is still open after the send-back goes into the approval itself.
func TestBeforeAnApprovalWhatStaysOpenGoesIntoTheApprovalNotAQuestion(t *testing.T) {
	stubWorkerTools(t, gatedGui, "", "")
	raised := `{"issues": [{"what": "Rate 18% is in no document read", "settle": "person"}]}`
	res, _ := hostedRun(t, []string{
		"COMMAND:\necho register row 455 CANCELLED", "COMMAND:\necho two", "COMMAND:\necho three", "COMMAND:\necho four",
		"COMMAND:\ngui click e35",
		"JUDGEMENTS:\n- Rate taken as 18% | not backed: earlier months", raised,
		"JUDGEMENTS:\n- Rate taken as 18% | not backed: earlier months", raised,
	})
	reason, _ := res.Meta["approvalReason"].(string)
	if res.Meta["needsApproval"] != true || res.Meta["needsUserInput"] == true ||
		!strings.Contains(reason, "Still open after an independent review") || !strings.Contains(reason, "Rate 18% is in no document read") {
		t.Fatalf("result = %+v", res.Meta)
	}
}

// Found live (2026-10-23): answered "use your judgement", the same point came back reworded in the next round.
func TestAPointPutToThePersonIsShownToLaterReviewsWithTheirAnswer(t *testing.T) {
	raised := `{"issues": [{"what": "The report claims it checked 198_sales_invoice", "settle": "recheck"}]}`
	o := &OperatorSkill{}
	// (The worker's own send-backs spent: a point it settles by looking is the person's only then.)
	st := &operatorState{Hosted: true, Request: "save the files", JudgementRounds: 1, SentBackRound: 1, WorkSendBacks: maxWorkSendBacks}
	if w := o.weighJudgements(weighRC(&scriptedChat{replies: []string{raised}}), st, nil, "Saved.", ""); w.question == "" {
		t.Fatalf("not asked: %+v", w)
	}
	rc := RunContext{Ctx: context.Background(), OnLog: func(string) {}, HumanReply: "Use your judgement and carry on."}
	st, _ = o.resume(rc, Subtask{Step: "save the files"}, st)
	facts := st.reviewFacts("Saved.", nil, "")
	if !strings.Contains(facts, "POINTS ALREADY PUT TO THE PERSON") || !strings.Contains(facts, "198_sales_invoice") ||
		!strings.Contains(facts, "Their answer: Use your judgement") {
		t.Fatalf("facts: %s", facts)
	}
}

// flakyChat times out once, then answers from its script.
type flakyChat struct {
	scriptedChat
	failed bool
}

func (f *flakyChat) Chat(ctx context.Context, model string, messages []llm.Message, opts llm.Options) (llm.Result, error) {
	if !f.failed {
		f.failed = true
		return llm.Result{}, fmt.Errorf("openai: request failed: Post \"http://decurix:8080/api/llm/azure/v1/chat/completions\": %w", context.DeadlineExceeded)
	}
	return f.scriptedChat.Chat(ctx, model, messages, opts)
}

// Found live (2026-10-23): one model call past its deadline ended a half-filed month as a fatal failure.
func TestAModelCallThatTimedOutIsAskedAgain(t *testing.T) {
	defer func(base time.Duration) { modelRetryBase = base }(modelRetryBase)
	modelRetryBase = time.Millisecond
	if !transientModelError(&llm.HTTPError{Provider: "openai", Status: 502, Message: "unreachable"}) {
		t.Error("a temporary failure of the endpoint was not taken as transient")
	}
	chat := &flakyChat{scriptedChat: scriptedChat{replies: []string{"DONE:\nAll filed."}}}
	res, err := NewOperator().Run(operatorContext(t, chat), Subtask{Step: "say done", Type: "operator"})
	if err != nil || res.Output != "All filed." || res.Meta["fatal"] == true {
		t.Fatalf("result = %+v %v", res, err)
	}
	if transientModelError(&llm.HTTPError{Provider: "openai", Status: 400, Message: "invalid model"}) || transientModelError(errors.New("anything")) {
		t.Error("a refusal of the request was taken as transient")
	}
}

// A reply that did not grant a command is told to the model as what it decided: a condition to meet first, a reply
// that decides nothing (a question back), or a no (2026-10-23).
func TestAReplyThatGrantedNothingIsToldAsWhatItDecided(t *testing.T) {
	rc := func(r *Reading) RunContext { return RunContext{HumanReading: r} }
	cmd := "gui click e35"
	cond := notApprovedNote(rc(&Reading{Decision: "if", Condition: "the client's messages hold no cancellation"}), cmd, "ok provided chat has nothing")
	if !strings.Contains(cond, "only once something holds") || !strings.Contains(cond, "the client's messages hold no cancellation") ||
		!strings.Contains(cond, "see the result for yourself") || !strings.Contains(cond, "quotes what you saw") ||
		!strings.Contains(cond, "never DONE") {
		t.Errorf("condition note: %s", cond)
	}
	asked := notApprovedNote(rc(&Reading{Decision: "unclear"}), cmd, "did you check the chat?")
	if !strings.Contains(asked, "does not say yes or no") || !strings.Contains(asked, "HELP:") {
		t.Errorf("unclear note: %s", asked)
	}
	for _, r := range []*Reading{{Decision: "no"}, nil} {
		if no := notApprovedNote(rc(r), cmd, "no"); !strings.Contains(no, "did not approve") || !strings.Contains(no, "Do not run that command now") {
			t.Errorf("no note (%v): %s", r, no)
		}
	}
}

// Found live (2026-10-23): before a filing that was one step of three, the review raised that the later steps were not
// done. Before an action, what comes after it is not missing.
func TestBeforeAnActionTheStepsAfterItAreNotRaisedAsMissing(t *testing.T) {
	st := &operatorState{Request: "file the first return, then the second, then note both receipts"}
	facts := st.reviewFacts("ready to file the first", nil, `click button "FILE" (e8) in "Portal"`)
	if !strings.Contains(facts, "parts of the request that come after it are not done yet") {
		t.Errorf("facts: %s", facts)
	}
}

func TestAQuoteIsFoundOnlyAsWholeWords(t *testing.T) {
	seen := " " + plainWords("the rate 50 applies to bills of September and October") + " "
	if quoteSeen("the rate 5 applies to bills", seen) {
		t.Error("\"rate 5\" was found in \"rate 50\"")
	}
	if !quoteSeen("the rate 50 applies to bills", seen) {
		t.Error("the whole quote was not found")
	}
}

func TestThePersonsWordsOutliveEveryCompaction(t *testing.T) {
	st := longGoal(30)
	st.PersonSaid = []string{"Asked: which rate?\n  They said: the rent is paid in cash"}
	o, rc := &OperatorSkill{}, RunContext{Ctx: context.Background(), Provider: &notesProvider{}, OnLog: func(string) {}}
	o.compact(rc, st, "first")
	for i := 0; i < 30; i++ {
		st.Messages = append(st.Messages, llm.Message{Role: "assistant", Content: "COMMAND:\necho x"},
			llm.Message{Role: "user", Content: "exit_code: 0"})
	}
	o.compact(rc, st, "second")
	if note := st.Messages[st.prefixEnd()].Content; !strings.Contains(note, "the rent is paid in cash") {
		t.Fatalf("the second compaction lost what the person said: %q", note)
	}
	if facts := st.reviewFacts("done", nil, ""); !strings.Contains(facts, "the rent is paid in cash") {
		t.Error("the reviewer was not shown what the person said")
	}
}

// Found live: figures read off a screenshot are in no text; the reviewer is shown the pictures the worker last saw.
func TestTheReviewerIsShownThePicturesTheWorkerLastSaw(t *testing.T) {
	st := &operatorState{Vision: true, Request: "check the ledger", Messages: []llm.Message{{Role: "user", Content: "x"},
		{Role: "user", Content: "old", Images: [][]byte{[]byte(pngHead + "1")}, ImageLabels: []string{"old screen"}},
		{Role: "user", Content: "new", Images: [][]byte{[]byte(pngHead + "2")}, ImageLabels: []string{"the screen as this command left it"}}}}
	chat := &scriptedChat{replies: []string{`{"issues": []}`}}
	if _, ok := (&OperatorSkill{}).review(weighRC(chat), st, "done", nil, ""); !ok {
		t.Fatal("not reviewed")
	}
	sent := chat.calls[0][1]
	if len(sent.Images) != 2 || string(sent.Images[1]) != pngHead+"2" || sent.ImageLabel(1) != "the screen as this command left it" {
		t.Fatalf("pictures sent: %d %v", len(sent.Images), sent.ImageLabels)
	}
	st.Vision = false
	if imgs, _ := st.lastPictures(3); len(imgs) != 0 {
		t.Error("a model that cannot see was sent pictures")
	}
}

func TestTheReviewerSeesTheLessonsOfALongFrame(t *testing.T) {
	frame := "intro\n" + strings.Repeat("persona and tools text. ", 2000) + "\nLESSON: rent under reverse charge is paid in cash."
	st := &operatorState{Request: "file it", PrefixLen: 1, Messages: []llm.Message{{Role: "system", Content: frame}}}
	if facts := st.reviewFacts("done", nil, ""); !strings.Contains(facts, "rent under reverse charge is paid in cash") {
		t.Error("the lessons at the end of a long frame were cut off")
	}
}

// Found live (2026-10-23): invoice lines written from memory into a script, printed back, were taken as "seen".
func TestWhatACommandPrintsBackOfItsOwnTextIsNotASource(t *testing.T) {
	command := "python3 - <<'PY'\nrows = [{\"inv\": \"454\", \"hsn\": \"0910\", \"taxable\": \"36000\"}]\nprint(rows, sum(1 for r in rows) * 1990)\nPY"
	got := withoutCommandValues("[{'inv': '454', 'hsn': '0910', 'taxable': '36000'}] 1990 3980", command)
	if strings.Contains(got, "0910") || strings.Contains(got, "36000") {
		t.Errorf("a value the command was given passed as seen: %q", got)
	}
	if !strings.Contains(got, "3980") {
		t.Errorf("a value the command computed was dropped: %q", got)
	}
	if out := withoutCommandValues("Rice 1006 qty 650", "gui observe"); out != "Rice 1006 qty 650" {
		t.Errorf("a screen read lost its values: %q", out)
	}
}

// Found live (2026-10-25): sent back once early in a round, the worker keyed a whole return; the points the review found
// in that new work went straight to the person. Work that changed something since the last send-back is sent back
// again (bounded); only reading to settle the points is not.
func TestPointsFoundInWorkDoneSinceTheLastSendBackGoBackToTheWorkerFirst(t *testing.T) {
	raised := `{"issues": [{"what": "Row 3 holds figures of three bills", "settle": "recheck"}]}`
	o := &OperatorSkill{}
	weigh := func(worked []string, extra int) (weighing, *operatorState) {
		st := &operatorState{Hosted: true, Request: "enter the month", JudgementRounds: 1, SentBackRound: 1,
			SentBackWorked: 1, ExtraSendBacks: extra, Worked: worked, WorkSendBacks: maxWorkSendBacks}
		return o.weighJudgements(weighRC(&scriptedChat{replies: []string{raised}}), st, nil, "Entered.", ""), st
	}
	w, st := weigh([]string{"cat bills.txt", `gui type e12 "7950"`}, 0)
	if w.nudge == "" || w.question != "" || st.ExtraSendBacks != 1 || st.SentBackWorked != 2 {
		t.Fatalf("new typing since the send-back: %+v %+v", w, st)
	}
	if w, _ := weigh([]string{"cat bills.txt", "cat chat.txt"}, 0); w.question == "" || w.nudge != "" {
		t.Fatalf("only reading since the send-back was sent back again: %+v", w)
	}
	if w, _ := weigh([]string{"cat bills.txt", `gui type e12 "7950"`}, maxExtraSendBacks); w.question == "" {
		t.Fatalf("sent back past the bound: %+v", w)
	}
}

// Found live (2026-10-25): two refusals of other values, each put right, used a goal-wide budget, and the next
// untraced value - a sum the worker had not printed - went to the person at once. The count starts again once the work
// types what it can trace; a worker that keeps guessing still reaches the person after two.
func TestRefusalsOfUntracedValuesAreCountedSinceTheLastTracedTyping(t *testing.T) {
	st := &operatorState{}
	if !st.untracedSendBack() || !st.untracedSendBack() || st.untracedSendBack() {
		t.Fatal("guessing: two send-backs, then the person")
	}
	st.noteTracedTyping("gui click e4") // no typing: nothing traced
	if st.untracedSendBack() {
		t.Fatal("a click reset the count")
	}
	st.noteTracedTyping(`gui type e5 "47940"`)
	if !st.untracedSendBack() {
		t.Fatal("after typing a traced value, the next untraced one went to the person at once")
	}
}
