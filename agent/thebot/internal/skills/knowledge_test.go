package skills

import (
	"context"
	"strings"
	"testing"
)

// Found live (2026-10-22): calls that quoted real words which did not establish them.

func weighRC(chat *scriptedChat) RunContext {
	return RunContext{Ctx: context.Background(), Provider: chat, Model: "m", OnLog: func(string) {}}
}

func TestACallBackedOnlyByAWebPageIsPutToThePersonAsASource(t *testing.T) {
	st := &operatorState{Hosted: true, Request: "file the GSTR-1", Seen: "invoices"}
	st.noteLookup(`lookup "GSTR-1 documents issued cancelled invoices"`,
		"https://example.org/gstr1-help\nCancelled invoices are reported in Table 13 in the cancelled column and counted in the total.")
	items := []string{`Counted bill 455 as cancelled | backed by: "reported in Table 13 in the cancelled column" (https://example.org/gstr1-help)`}
	w := (&OperatorSkill{}).weighJudgements(weighRC(&scriptedChat{replies: []string{`{"issues": []}`}}), st, items, "Filed.", "")
	if w.nudge != "" || !strings.Contains(w.question, "Is each a source you trust") || !strings.Contains(w.question, "https://example.org/gstr1-help") {
		t.Fatalf("weighing %+v", w)
	}
	// Their yes makes the page a source for the rest of the goal; a no leaves it out.
	st.settleAskedSources("yes, that is the official help", true)
	if !strings.Contains(st.Seen, "Table 13") || len(st.AskedSources) != 0 {
		t.Error("a vouched page did not become a source")
	}
	other := &operatorState{AskedSources: []int{0}, Lookups: st.Lookups}
	other.settleAskedSources("no, use our rules", false)
	if strings.Contains(other.Seen, "Table 13") {
		t.Error("a refused page became a source")
	}
	// A reply with a link of its own points elsewhere: the looked-up page is not vouched for.
	elsewhere := &operatorState{AskedSources: []int{0}, Lookups: st.Lookups}
	elsewhere.settleAskedSources("use https://example.org/our-manual.pdf instead", true)
	if strings.Contains(elsewhere.Seen, "Table 13") {
		t.Error("a page the person passed over for their own link became a source")
	}
}

// What the review raises is sent back once a round, then asked, and past the questions a goal may ask, carried with the
// result - never dropped (review of 2026-10-22).
func TestWhatTheReviewRaisesIsSentBackThenAskedThenCarriedNeverDropped(t *testing.T) {
	raised := `{"issues": [{"what": "Invoice 458 never read", "why": "the chat offered it", "settle": "person"}]}`
	chat := &scriptedChat{replies: []string{raised, raised, raised, raised}}
	o, rc := &OperatorSkill{}, weighRC(chat)
	st := &operatorState{Hosted: true, Request: "file it", JudgementRounds: 1}
	if w := o.weighJudgements(rc, st, nil, "Filed.", ""); w.nudge == "" {
		t.Fatalf("not sent back first: %+v", w)
	}
	if w := o.weighJudgements(rc, st, nil, "Filed.", ""); w.question == "" || !strings.Contains(w.question, "nothing you gave me or taught me settles") {
		t.Fatalf("not asked once sent back: %+v", w)
	}
	// A later round, the person already asked as often as they may be.
	st.JudgementRounds, st.PersonAsks = 2, maxReviewQuestions
	if w := o.weighJudgements(rc, st, nil, "Filed.", ""); w.nudge == "" {
		t.Fatalf("a new round's points were not sent back: %+v", w)
	}
	w := o.weighJudgements(rc, st, nil, "Filed.", "")
	if w.nudge != "" || w.question != "" || len(w.open) != 1 || !strings.Contains(w.open[0], "Invoice 458 never read") {
		t.Fatalf("what is still open was not carried: %+v", w)
	}
	if note := openPointsNote(st, w.open); !strings.Contains(note, "Still open after an independent review") {
		t.Errorf("note: %q", note)
	}
	// The second review is shown what the first raised.
	if facts := chat.calls[3][1].Content; !strings.Contains(facts, "POINTS AN EARLIER REVIEW RAISED") {
		t.Errorf("the second review did not see the first one's points")
	}
}

// Found live (2026-10-23): a point the worker could settle by looking and did not is put to the person, as what it
// is - carried silently, it left a month's work wrong.
func TestAPointTheWorkerDidNotSettleIsPutToThePerson(t *testing.T) {
	raised := `{"issues": [{"what": "Invoice 457 (a photo) was never read", "settle": "recheck"}]}`
	o, rc := &OperatorSkill{}, weighRC(&scriptedChat{replies: []string{raised, raised}})
	st := &operatorState{Hosted: true, Request: "file it", JudgementRounds: 1}
	if w := o.weighJudgements(rc, st, nil, "Filed.", ""); w.nudge == "" {
		t.Fatalf("not sent back first: %+v", w)
	}
	w := o.weighJudgements(rc, st, nil, "Filed.", "")
	if w.question == "" || !strings.Contains(w.question, "still questions these") || !strings.Contains(w.question, "Invoice 457") {
		t.Fatalf("a point the worker left open was not put to the person: %+v", w)
	}
}

func TestAReviewThatCannotBeMadeIsSaid(t *testing.T) {
	st := &operatorState{Hosted: true, Request: "file it", JudgementRounds: 1, PersonAsks: maxReviewQuestions}
	w := (&OperatorSkill{}).weighJudgements(weighRC(&scriptedChat{replies: []string{"no json", "still none"}}), st, nil, "Filed.", "")
	if len(w.open) != 1 || w.open[0] != unreviewed {
		t.Fatalf("an unmade review was not said: %+v", w)
	}
}

func TestTheQuestionToThePersonCarriesNoSecretTheRequestGave(t *testing.T) {
	st := &operatorState{Hosted: true, Request: "log in with password Hunter#2026 and file", JudgementRounds: 1, SentBackRound: 1}
	raised := `{"issues": [{"what": "Typed Hunter#2026 into the user field", "settle": "person"}]}`
	w := (&OperatorSkill{}).weighJudgements(weighRC(&scriptedChat{replies: []string{raised}}), st, nil, "Filed.", "")
	if w.question == "" || strings.Contains(w.question, "Hunter#2026") {
		t.Fatalf("question: %q", w.question)
	}
}

func TestAPageThePersonLinkedIsASource(t *testing.T) {
	st := &operatorState{}
	st.notePersonLinks("Follow https://example.org/office-rules.pdf for how we treat rent.")
	if !st.fetchesPersonLink(`lookup "https://example.org/office-rules.pdf"`) {
		t.Error("the person's own link was not taken as theirs")
	}
	if st.fetchesPersonLink(`lookup "GST rent reverse charge"`) {
		t.Error("a search the person did not give was taken as theirs")
	}
}

func TestTheAnswerAboutTheCallsIsAppliedBeforeAnythingElse(t *testing.T) {
	st := &operatorState{Hosted: true, JudgementQuestion: true, JudgedAt: 12, CommandsSucceeded: 12,
		JudgementRounds: 1, DoneRounds: 1, LastAsked: "Before I report this as done:\n1. Documents issued: 0 cancelled"}
	rc := RunContext{Ctx: context.Background(), OnLog: func(string) {}, HumanReply: "A cancelled invoice still counts in documents issued."}
	st, blocked := (&OperatorSkill{}).resume(rc, Subtask{Step: "file it"}, st)
	if blocked != "" {
		t.Fatal(blocked)
	}
	last := st.Messages[len(st.Messages)-1].Content
	if !strings.Contains(last, "A cancelled invoice still counts") || !strings.Contains(last, "Apply it before anything else") ||
		!strings.Contains(last, "not only the points listed") || !strings.Contains(last, "The question: Before I report this as done") {
		t.Errorf("message: %q", last)
	}
	// Their answer is weighed before any approval; a report review waits for work to follow (it reviews what changed).
	if st.JudgementQuestion || !st.judgementsDue("approval") {
		t.Error("the calls are not weighed again before an approval after the answer")
	}
	if st.judgementsDue("done") {
		t.Error("a report review was due with nothing done since the answer")
	}
	st.CommandsSucceeded++
	if !st.judgementsDue("done") {
		t.Error("a report review was not due once work followed the answer")
	}
	// The points put to them, with their answer, are shown to every later review as settled.
	if facts := st.reviewFacts("done", nil, ""); len(st.AnsweredPoints) != 0 && !strings.Contains(facts, "POINTS ALREADY PUT TO THE PERSON") {
		t.Error("answered points not shown to the review")
	}
	// What they said outlives the transcript, with what it answered.
	if len(st.PersonSaid) != 1 || !strings.Contains(st.PersonSaid[0], "0 cancelled") || !strings.Contains(st.PersonSaid[0], "still counts") {
		t.Errorf("person said: %q", st.PersonSaid)
	}
	if !strings.Contains(st.Seen, "still counts in documents issued") {
		t.Error("the answer is not a source")
	}
}

// The agent never answers a question about the run's own work from lessons (skill_task.WORK_CHECKED matches this
// wording): changing it here means changing it there.
func TestTheReviewsPointsAreAskedInTheWordsTheAgentKnows(t *testing.T) {
	q := knowledgeQuestion([]reviewIssue{{What: "Bill 457 was not entered", Settle: "recheck"}}, nil, "")
	if !strings.Contains(q, "An independent check of the work still questions") {
		t.Fatalf("question = %s", q)
	}
}
