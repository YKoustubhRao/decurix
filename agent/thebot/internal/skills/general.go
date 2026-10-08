package skills

import (
	"regexp"

	"thebot/internal/llm"
	"thebot/internal/prompt"
)

// operatorMarkerRe is a reply that opens the way the operator's do: a model
// that has been told how tasks are worked, answering one in conversation.
// "Done." in any case is the same claim: found live, a request to correct an application record reached this skill and came
// back "Done. I updated the record ... and verified the date now shows", nothing having been done.
var operatorMarkerRe = regexp.MustCompile(`^\s*\**(DONE|COMMAND)\b\**\s*(:|$)|(?i)^\s*\**done\b\**\s*[.!:]`)

// GeneralSkill is the conversational catch-all - real, basic chat was
// missing entirely until this was added: every request that didn't match
// terminal/rag-query/feedback's specific classifiers fell through to task
// type "general", which had NO skill registered for it at all, so even
// "hello" or "what is 1+1?" failed with "no skill available". This is
// simply the LLM's own direct response to the request - no tool use, no
// file writes.
//
// Real bug found live and fixed here: this used to send ONLY the current
// request with zero prior context, so a second request in the same UI
// session had no idea the first one ever happened ("what's my name?"
// right after "my name is X" got an honest "I don't have that" - the
// model wasn't lying, it was never given anything to remember with).
// rc.SessionHistory (internal/session) is now threaded in as real prior
// turns. SYSTEM_PREAMBLE exists so the model doesn't over-promise
// capabilities that don't exist yet (cross-session/persistent memory) now
// that within-session recall is real.
type GeneralSkill struct{}

func NewGeneral() *GeneralSkill { return &GeneralSkill{} }

func (g *GeneralSkill) Name() string { return "general" }
func (g *GeneralSkill) Description() string {
	return "Answers directly via the LLM - the conversational fallback for anything no other skill claims."
}

func (g *GeneralSkill) Matches(subtask Subtask) bool {
	return subtask.Type == "general"
}

// systemPreamble is TheBot's persona - now the head of the shared system
// frame (internal/prompt) that every model call in a job opens with, so a
// conversational answer and the operator's command loop read it from the
// same cached prefix.
const systemPreamble = prompt.Persona

func (g *GeneralSkill) Run(rc RunContext, subtask Subtask) (Result, error) {
	// Self-stats and recalled facts ride inside the frame itself.
	messages := make([]llm.Message, 0, len(rc.SessionHistory)+3)
	system := rc.SystemMessage()
	system.Content += "\n\n" + prompt.ChatRule
	messages = append(messages, system)

	// What earlier steps of this same task already found. Without this a
	// later step answers from training data and contradicts a search that
	// just ran two seconds earlier.
	if findings := rc.PriorFindings(); findings != "" {
		messages = append(messages, llm.Message{
			Role: "system",
			Content: "Earlier steps of this task already established the following, from real sources checked just now. " +
				"Treat them as current fact and build on them - they override anything you remember from training:\n\n" + findings,
		})
	}

	messages = append(messages, rc.SessionHistory...)
	messages = append(messages, llm.Message{Role: "user", Content: subtask.Step})

	resp, err := rc.Provider.Chat(rc.Ctx, rc.Model, messages, llm.Options{TimeoutMs: 120000})
	if err != nil {
		return Result{
			Subtask: subtask, SkillName: g.Name(),
			Output: "Could not get a response: " + err.Error(), Verified: false, Meta: map[string]any{"error": err.Error()},
		}, nil
	}

	// The model decided it cannot answer without looking at the machine.
	// Not a failure and not an answer: the scheduler hands the same step to
	// the operator, which can look.
	if prompt.IsNeedsTools(resp.Text) {
		rc.Logf("general", "this needs a look at the machine - handing it to the operator")
		return Result{
			Subtask: subtask, SkillName: g.Name(),
			Output: "Needs to look at this machine to answer.", Verified: false,
			Meta: map[string]any{"reroute": "operator"},
		}, nil
	}

	// A reply in the operator's own words - "DONE", "COMMAND: ..." - is a
	// model that took the request for work and has no tools to do it with.
	// Found live: a taught screen task reached this skill, which answered
	// "DONE", and that was reported as the task's result with nothing done.
	// It is work: the operator gets it.
	if operatorMarkerRe.MatchString(resp.Text) {
		rc.Logf("general", "this is work, not a question - handing it to the operator")
		return Result{
			Subtask: subtask, SkillName: g.Name(),
			Output: "Needs to be done on this machine.", Verified: false,
			Meta: map[string]any{"reroute": "operator"},
		}, nil
	}

	return Result{
		Subtask: subtask, SkillName: g.Name(),
		Output: resp.Text, Verified: true, Meta: map[string]any{"userFacing": true},
	}, nil
}
