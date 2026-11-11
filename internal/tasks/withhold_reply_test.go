package tasks

import (
	"strings"
	"testing"
)

// A bare answer to a question that asked for a secret is not kept in the records (review 2026-11-03: "552901"
// answering "What is the OTP?" was stored as it was typed); an answer to any other question is kept as typed.
func TestAReplyToASecretQuestionIsWithheld(t *testing.T) {
	c := newClock()
	s := openStore(t, c)
	box := worker("box", "claude")

	route := send(t, s, box, "file the return on the portal")
	outcome(t, s, "box", route.TaskID, Outcome{Kind: OutcomeQuery, Reply: "What is the OTP the client received?"})
	send(t, s, box, "552901")
	outcome(t, s, "box", route.TaskID, Outcome{Kind: OutcomeQuery, Reply: "Which period, October or November?"})
	send(t, s, box, "October 2026")

	interventions := get(t, s, route.TaskID).Executions[0].Interventions
	if len(interventions) != 2 {
		t.Fatalf("got %d interventions, want 2", len(interventions))
	}
	if got := interventions[0].Reply; got != "[withheld]" {
		t.Errorf("the OTP answer was kept as %q", got)
	}
	if got := interventions[1].Reply; got != "October 2026" {
		t.Errorf("an ordinary answer was kept as %q", got)
	}
}

// A reply that names the OTP while explaining the work is over-withheld in the records - every word of it with a digit
// or a mark - however long it is: "the OTP is 552901, go ahead" kept the code (review of freeze 2, A3: a five-word
// limit). What the desktop is told is a secret rests on structure (MarksSecret): the person's mark, a question asked for
// a secret, or - for a question any engine but the learner asked - whatever the records withhold. A lesson's reply to a
// read-back that names the OTP is not marked, so the lesson is not held as secrets on the desktop.
func TestALongReplyNamingTheOTPIsWithheldInTheRecords(t *testing.T) {
	question := "What is the OTP the client received?"
	for _, reply := range []string{"the OTP is 552901, go ahead", "552901\nand the next one comes tomorrow",
		"ok so the code the client read out to me on the phone just now is 552901 - go ahead and file"} {
		stored, withheld := ReplyForm(reply, false, question, false)
		if !withheld || strings.Contains(stored, "552901") {
			t.Errorf("%q was kept as %q (withheld %v)", reply, stored, withheld)
		}
	}
	if stored, withheld := ReplyForm("ask me later", false, question, true); !withheld || stored != "[secret withheld]" {
		t.Errorf("a reply to a question asked for a secret was kept as %q (withheld %v)", stored, withheld)
	}
}

func TestOnlyStructureMarksALessonReplySecret(t *testing.T) {
	c := newClock()
	s := openStore(t, c)
	box := worker("box", "claude")
	readBack := "Stop before File with EVC so the person can provide the OTP sent to the client's phone. Is that right?"
	lesson := "Mostly right. Only the client has the OTP, never guess. Amounts are tax-inclusive: taxable is the " +
		"amount times 100/118 above 1,00,000."

	// The learner's read-back, not asked for a secret: the records withhold its figures, the desktop is not told.
	taught := send(t, s, box, "learn gstr1 from the recording")
	outcome(t, s, "box", taught.TaskID, Outcome{Kind: OutcomeQuery, Engine: LearnEngine, Reply: readBack})
	reply := Route{Kind: RouteReply, TaskID: taught.TaskID, ExecutionID: taught.ExecutionID}
	if _, withheld := s.StoredForm(reply, lesson, false); !withheld {
		t.Fatalf("the records keep the lesson reply whole; the over-withholding this test rests on is gone")
	}
	if s.MarksSecret(reply, lesson, false) {
		t.Errorf("a lesson reply to a read-back not asked for a secret was marked secret")
	}
	if !s.MarksSecret(reply, lesson, true) {
		t.Errorf("a lesson reply the person marked was not marked secret")
	}

	// The same words to a worker's question: marked, as the records withhold them.
	work := send(t, s, box, "file the return on the portal")
	outcome(t, s, "box", work.TaskID, Outcome{Kind: OutcomeQuery, Engine: "thebot", Reply: readBack})
	asked := Route{Kind: RouteReply, TaskID: work.TaskID, ExecutionID: work.ExecutionID}
	if !s.MarksSecret(asked, lesson, false) {
		t.Errorf("a reply to a worker's question naming the OTP, with figures withheld, was not marked secret")
	}

	// The learner's question asked for a secret: marked.
	secret := send(t, s, box, "learn the portal sign-in")
	outcome(t, s, "box", secret.TaskID, Outcome{Kind: OutcomeQuery, Engine: LearnEngine, Secret: true,
		Reply: "What is the portal password?"})
	if !s.MarksSecret(Route{Kind: RouteReply, TaskID: secret.TaskID, ExecutionID: secret.ExecutionID}, "Lift$2026", false) {
		t.Errorf("a reply to a learner's question asked for a secret was not marked secret")
	}
}
