package scheduler

import (
	"strings"
	"testing"

	"thebot/internal/skills"
	"thebot/internal/withheld"
)

func TestL2KeepsNoSecretValue(t *testing.T) {
	secrets := withheld.Clean([]string{"plumtree-cabinet-41"})
	job := &Job{Prompt: "use plumtree-cabinet-41 to sign in", Outcome: OutcomeAnswered,
		Plan: []skills.Subtask{{Step: "type plumtree-cabinet-41 into the password field", Type: "operator"}}}
	e := l2Entry(secrets, job)
	if strings.Contains(e.Prompt+strings.Join(e.Plan, ""), "plumtree") || !strings.Contains(e.Prompt, "sign in") {
		t.Fatalf("l2 entry = %+v", e)
	}
	whole := l2Entry(withheld.Clean([]string{"my whole request"}), &Job{Prompt: "my whole request", Outcome: OutcomeAnswered,
		Plan: []skills.Subtask{{Step: "my whole request"}}})
	if whole.Prompt != withheld.Mark || len(whole.Plan) != 0 {
		t.Fatalf("a wholly secret request kept %+v", whole)
	}
}
