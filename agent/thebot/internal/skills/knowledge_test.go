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
	// A later round, with work done since (an unchanged point is not sent again), the person already asked as often as
	// they may be.
	st.JudgementRounds, st.PersonAsks = 2, maxReviewQuestions
	st.Changes++
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
	// (Since 2026-10-27 such a point is the worker's for maxWorkSendBacks send-backs first - with those spent:)
	st := &operatorState{Hosted: true, Request: "file it", JudgementRounds: 1, WorkSendBacks: maxWorkSendBacks}
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

// Found live (2026-10-26): four bills declared and never saved were put to the person as "choices I made", twice, after
// they had said "enter them". A point decided in code that is the worker's to do goes back to the worker, bounded, and
// past the bound is carried open - never asked.
func TestAPointThatIsTheWorkersToDoIsNeverPutToThePerson(t *testing.T) {
	none := `{"issues": []}`
	chat := &scriptedChat{replies: []string{none, none, none, none, none, none}}
	o, rc := &OperatorSkill{}, weighRC(chat)
	st := &operatorState{Hosted: true, Request: "enter the bills", JudgementRounds: 1, RecordLedger: sampleLedger}
	if w := o.weighJudgements(rc, st, nil, "Entered.", ""); w.nudge == "" || w.question != "" {
		t.Fatalf("not sent back first: %+v", w)
	}
	for n := 0; n < maxWorkSendBacks; n++ {
		st.Changes++ // the worker did something since (a point sent with nothing done since is not sent again)
		w := o.weighJudgements(rc, st, nil, "Entered.", "")
		if w.question != "" || !strings.Contains(w.nudge, "yours to settle, not the person's") || !strings.Contains(w.nudge, "record 1190 was declared from its document but never saved") {
			t.Fatalf("send-back %d: %+v", n+1, w)
		}
	}
	w := o.weighJudgements(rc, st, nil, "Entered.", "")
	if w.question != "" || w.nudge != "" || len(w.open) != 2 || !strings.Contains(strings.Join(w.open, "\n"), "never saved") {
		t.Fatalf("past the bound the work points were not carried open: %+v", w)
	}
	if st.PersonAsks != 0 {
		t.Errorf("the person was asked %d time(s)", st.PersonAsks)
	}
}

func TestAPointThePersonAnsweredIsNotAskedAgain(t *testing.T) {
	raised := `{"issues": [{"what": "Invoice 458 never read", "why": "the chat offered it", "settle": "person"}]}`
	chat := &scriptedChat{replies: []string{raised, raised, raised}}
	o, rc := &OperatorSkill{}, weighRC(chat)
	st := &operatorState{Hosted: true, Request: "file it", JudgementRounds: 1,
		AnsweredKeys: map[string]string{pointKey("Invoice 458 never read - the chat offered it"): "Read it and enter it - it is this month's."}}
	if w := o.weighJudgements(rc, st, nil, "Filed.", ""); w.nudge == "" {
		t.Fatalf("not sent back first: %+v", w)
	}
	w := o.weighJudgements(rc, st, nil, "Filed.", "")
	if w.question != "" || !strings.Contains(w.nudge, "already answered this point") || !strings.Contains(w.nudge, "Read it and enter it") {
		t.Fatalf("the answered point was asked again or not sent back with the answer: %+v", w)
	}
	if k := pointKey("Invoice 458 NEVER read  - the chat offered it"); k != "invoice 458 never read" {
		t.Errorf("pointKey = %q", k)
	}
}

func TestAnAdvisoryPointGoesNoFurtherThanTheSendBack(t *testing.T) {
	issues := []reviewIssue{{What: "a", Advisory: true}, {What: "b"}}
	if got := notAdvisory(issues); len(got) != 1 || got[0].What != "b" {
		t.Errorf("notAdvisory = %+v", got)
	}
}

// Found live (2026-10-27): points the worker settles by looking went to the person, who answered "open it and look".
func TestAPointSettledByLookingStaysTheWorkersWhileSendBacksRemain(t *testing.T) {
	raised := `{"issues": [{"what": "The report says the Day Book shows six vouchers, but no look this goal showed it", "why": "nothing on screen", "settle": "recheck"}]}`
	chat := &scriptedChat{replies: []string{raised, raised, raised, raised, raised, raised}}
	o, rc := &OperatorSkill{}, weighRC(chat)
	st := &operatorState{Hosted: true, Request: "enter the bills", JudgementRounds: 1}
	for n := 0; n < maxWorkSendBacks+1; n++ {
		w := o.weighJudgements(rc, st, nil, "Entered.", "")
		if w.question != "" {
			t.Fatalf("asked at round %d while send-backs remained: %q", n, w.question)
		}
		if w.nudge == "" {
			t.Fatalf("not sent back at round %d: %+v", n, w)
		}
		st.Changes++ // the worker did something since (a point sent with nothing done since is not sent again)
	}
	w := o.weighJudgements(rc, st, nil, "Entered.", "")
	if w.question == "" || !strings.Contains(w.question, "Day Book") {
		t.Fatalf("past the bound the point was not put to the person: %+v", w)
	}
}

// Found live (2026-11-10): the same review point went back to the worker round after round with nothing done in
// between, and was carried with the result anyway; the operator's turns rose 67 -> 110 over four cases.
func TestAPointSentBackWithNothingDoneSinceIsNotSentAgainAndStaysOpen(t *testing.T) {
	o := &OperatorSkill{}
	// A point decided in code that is the worker's to do: a document read and never entered.
	st := &operatorState{Hosted: true, Request: "enter the bills", JudgementRounds: 1}
	st.addUnit("document", "Invoice_455.pdf", "Invoice 455 taxable 8,000.00")
	none := `{"issues": []}`
	rc := weighRC(&scriptedChat{replies: []string{none, none, none, none}})
	if w := o.weighJudgements(rc, st, nil, "Entered.", ""); !strings.Contains(w.nudge, "Invoice_455.pdf") {
		t.Fatalf("not sent back first: %+v", w)
	}
	// The worker only looked: the same point is not sent again, and is carried open - never dropped, never a pass.
	for _, c := range []string{"gui observe", "gui read e700", "office read /w/Invoice_455.pdf && gui observe"} {
		if !onlyLooks(c) {
			t.Fatalf("%q does more than look", c)
		}
	}
	w := o.weighJudgements(rc, st, nil, "Entered.", "")
	if w.nudge != "" || w.question != "" || !strings.Contains(strings.Join(w.open, "\n"), "Invoice_455.pdf") {
		t.Fatalf("an unchanged point was sent again, or not kept open: %+v", w)
	}
	// Something done since that could settle it: it is the worker's again.
	for _, c := range []string{`gui type e4 "8000"`, "gui click e9", "gui key ctrl+s", "python3 sum.py", "gui observe && gui click e3"} {
		if onlyLooks(c) {
			t.Fatalf("%q only looks", c)
		}
	}
	st.Changes++
	if w := o.weighJudgements(rc, st, nil, "Entered.", ""); !strings.Contains(w.nudge, "Invoice_455.pdf") {
		t.Fatalf("a point with work done since was not sent back: %+v", w)
	}

	// A point to recheck by looking, sent back and unchanged since, goes to the person - as past the send-backs.
	raised := `{"issues": [{"what": "The report says the Day Book shows six vouchers, but no look this goal showed it", "why": "nothing on screen", "settle": "recheck"}]}`
	st2 := &operatorState{Hosted: true, Request: "enter the bills", JudgementRounds: 1}
	rc2 := weighRC(&scriptedChat{replies: []string{raised, raised, raised}})
	if w := o.weighJudgements(rc2, st2, nil, "Entered.", ""); w.nudge == "" {
		t.Fatalf("not sent back first: %+v", w)
	}
	if w := o.weighJudgements(rc2, st2, nil, "Entered.", ""); w.nudge != "" || !strings.Contains(w.question, "Day Book") {
		t.Fatalf("an unchanged point to recheck was sent again, or not put to the person: %+v", w)
	}
	// A new round whose review raises it again, word for word, with nothing done since: not sent again either.
	st2.JudgementRounds, st2.PersonAsks = 2, maxReviewQuestions
	if w := o.weighJudgements(rc2, st2, nil, "Entered.", ""); w.nudge != "" || !strings.Contains(strings.Join(w.open, "\n"), "Day Book") {
		t.Fatalf("a new round sent an unchanged point again, or dropped it: %+v", w)
	}
}

func TestAPointForThePersonIsCutAtASentenceNeverMidWord(t *testing.T) {
	short := "Voucher 1188 names the wrong party."
	if forPerson(short, maxPointChars) != short {
		t.Fatal("a point that fits must stay whole")
	}
	long := strings.Repeat("The ledger was not saved after the person's yes. ", 30)
	got := forPerson(long, maxPointChars)
	if len(got) > maxPointChars+4 || !strings.HasSuffix(got, ". …") || strings.Contains(got, "[truncated]") {
		t.Fatalf("cut badly: %q", got[len(got)-40:])
	}
	q := knowledgeQuestion([]reviewIssue{{What: long, Settle: "recheck"}}, nil, "")
	if strings.Contains(q, "[truncated]") {
		t.Fatal("a question to the person shows an engineering marker")
	}
}
