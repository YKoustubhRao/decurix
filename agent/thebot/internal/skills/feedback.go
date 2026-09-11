package skills

import "fmt"

// FeedbackSkill records EXPLICIT negative feedback a user gives directly
// through chat - "source: user", distinct from the self-generated entries
// the scheduler records on its own unresolved failures, though both are
// consulted identically by reasonAboutFailure.
type FeedbackSkill struct{}

func NewFeedback() *FeedbackSkill { return &FeedbackSkill{} }

func (f *FeedbackSkill) Name() string { return "feedback" }
func (f *FeedbackSkill) Description() string {
	return "Records explicit negative feedback about the agent's own limitations, consulted on future failures of similar tasks."
}

func (f *FeedbackSkill) Matches(subtask Subtask) bool {
	return subtask.Type == "feedback"
}

func (f *FeedbackSkill) Run(rc RunContext, subtask Subtask) (Result, error) {
	if rc.Feedback == nil {
		return Result{
			Subtask: subtask, SkillName: f.Name(),
			Output: "No feedback store is configured for this session.", Verified: false, Meta: map[string]any{},
		}, nil
	}

	task := rc.OriginalPrompt
	if len(rc.PriorResults) > 0 {
		task = rc.PriorResults[len(rc.PriorResults)-1].Subtask.Step
	}

	if err := rc.Feedback.RecordNegative(rc.Ctx, task, subtask.Step, "user"); err != nil {
		return Result{
			Subtask: subtask, SkillName: f.Name(),
			Output: fmt.Sprintf("Could not record feedback: %s", err.Error()), Verified: false, Meta: map[string]any{"error": err.Error()},
		}, nil
	}

	rc.Logf("feedback", "recorded explicit user feedback: %s", subtask.Step)
	return Result{
		Subtask: subtask, SkillName: f.Name(),
		Output:   "Noted - recorded as negative feedback. I'll take this into account on similar tasks.",
		Verified: true, Meta: map[string]any{"recorded": true},
	}, nil
}
