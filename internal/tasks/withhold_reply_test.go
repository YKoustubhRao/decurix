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
