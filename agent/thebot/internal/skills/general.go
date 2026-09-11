package skills

import (
	"strings"

	"thebot/internal/llm"
)

// GeneralSkill is the conversational catch-all - real, basic chat was
// missing entirely until this was added: every request that didn't match
// terminal/rag-query/feedback's specific classifiers fell through to task
// type "general", which had NO skill registered for it at all, so even
// "hello" or "what is 1+1?" failed with "no skill available". This is
// simply the LLM's own direct response to the request - no tool use, no
// file writes.
//
// Real bug found live and fixed here: this used to send ONLY the current
// message with zero prior context, so a second message in the same UI
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

const systemPreamble = `You are TheBot, an AI agent running inside a VM. That is your name and identity - never call yourself anything else. The underlying language model answering this prompt is only your "brain": an interchangeable component you run on, not who you are, the same way a person isn't their neurons. If asked which model you are, you may name the underlying model honestly, but you are TheBot, not that model.

Be accurate about what you can actually do:
- You DO have this conversation's real history, turn by turn. If the user told you something earlier in this session, it is above - use it, never claim you can't see it.
- You DO have long-term memory across sessions. Important details (the user's name, their preferences, decisions they made) are stored and recalled automatically; anything recalled is given to you as a system message. When the user asks you to remember something, it really is stored - saying "I'll remember that" is true.
- If you were given no recalled facts and the user asks about something from a past session, say you don't have it rather than inventing it.
- You DO have real introspection into your own current state (how much is indexed in your Data RAG, how many facts you've stored, etc.) - when given below, it is real and current, queried just now. Never say you "don't have the ability to check" when that data is right there.
- You have real tools for other kinds of requests (running real shell commands, searching indexed documents, fetching web pages). If a request needs one of those, say so plainly rather than pretending you already did it.`

func (g *GeneralSkill) Run(rc RunContext, subtask Subtask) (Result, error) {
	messages := make([]llm.Message, 0, len(rc.SessionHistory)+4)
	messages = append(messages, llm.Message{Role: "system", Content: systemPreamble})

	if rc.SelfStats != "" {
		messages = append(messages, llm.Message{Role: "system", Content: rc.SelfStats})
	}

	// What L1 already knows, stated as fact rather than as a suggestion -
	// this is real remembered content, not a guess.
	if len(rc.RecalledFacts) > 0 {
		messages = append(messages, llm.Message{
			Role:    "system",
			Content: "Things you already know about this user, recalled from your long-term memory. Treat these as true and use them directly:\n- " + strings.Join(rc.RecalledFacts, "\n- "),
		})
	}

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

	resp, err := rc.Provider.Chat(rc.Ctx, rc.Model, messages, llm.Options{TimeoutMs: 90000})
	if err != nil {
		return Result{
			Subtask: subtask, SkillName: g.Name(),
			Output: "Could not get a response: " + err.Error(), Verified: false, Meta: map[string]any{"error": err.Error()},
		}, nil
	}
	return Result{
		Subtask: subtask, SkillName: g.Name(),
		Output: resp.Text, Verified: true, Meta: map[string]any{},
	}, nil
}
