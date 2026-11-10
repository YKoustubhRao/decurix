package tasks

import "testing"

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

// A lesson reply that explains the work - naming the OTP, with codes, figures, hyphenated words and possessives - is
// kept as typed, and so the desktop is not told it is secret (C14): only a question asked for a secret (its "secret",
// by structure) makes a reply one. Found live 2026-11-10: the read-back named "the OTP", every word of the reply with
// a digit or a mark was withheld, and the desktop held them all as secrets.
func TestALessonReplyNamingTheOTPIsNotASecret(t *testing.T) {
	question := "Stop before File with EVC so the person can provide the OTP sent to the client's phone. Is that right?"
	reply := "Mostly right. Only the client has the OTP: ask me for it, never guess. A receiver's GSTIN goes to " +
		"Table 4A B2B; an inter-state sale above 1,00,000 to B2C; amounts are tax-inclusive, taxable is the amount " +
		"times 100/118 (the app shows each), never made-up rows."
	stored, withheld := ReplyForm(reply, false, question, false)
	if withheld || stored != reply {
		t.Errorf("the lesson reply was kept as %q (withheld %v)", stored, withheld)
	}
	if stored, withheld := ReplyForm(reply, false, question, true); !withheld || stored != "[secret withheld]" {
		t.Errorf("a reply to a question asked for a secret was kept as %q (withheld %v)", stored, withheld)
	}
	if stored, withheld := ReplyForm(reply, true, question, false); !withheld || stored != "[secret withheld]" {
		t.Errorf("a reply the person marked secret was kept as %q (withheld %v)", stored, withheld)
	}
}
