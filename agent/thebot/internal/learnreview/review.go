package learnreview

// The review's one instruction, the messages it is sent with, and soul's reading of the answer. The answer is only
// read here: soul marks which evidence quotes stand in what the run read; whether a proposal is staged is the Python
// agent's code checks and the control plane's, and whether anything is learned is a person's.

import (
	"encoding/json"
	"strings"

	"thebot/internal/llm"
)

// Kinds of proposal. Few and generic: what the person or the work wants, and how an application is operated.
const (
	KindLesson  = "lesson"
	KindAppNote = "app_note"
)

// Bounds on what the review may propose.
const (
	MaxProposals   = 3
	MaxEvidence    = 3
	MaxTextChars   = 600
	MaxWhyChars    = 400
	MaxQuoteChars  = 500
	MaxReplaceChar = 4000
)

// Instruction is the one message appended to the run's transcript. Adapted from the "do not capture" rules of a
// background review elsewhere, with the opposite bias: proposing nothing is the expected answer, and a person decides.
const Instruction = `The task above is finished and its result was checked. One more question before you leave it: does this run show something a LATER task of this worker should know, which nothing you were given already says? If so, propose it. A person reads every proposal and decides; nothing is learned until they approve it. Proposing nothing is a fine and common answer.

Two kinds, no others:
- "lesson": about the person you work for, or the work itself - how they want this kind of work done, laid out or checked - as they said it in this run.
- "app_note": how an application or page you used is operated - where a control is, which step must come before another, what a screen needs before it saves - as a command's result in this run showed it working.

Each proposal stands on this run alone:
- Only what a person's words or a command's result above shows. Give the exact words as "evidence": one to three quotes, each copied character for character from what the person said or what a command printed. Never your own words, never a quote reworded or joined from two places.
- Never a trait of the person they did not state.
- Never a one-off request made into a standing rule: what they asked for this time is not how they always want it, unless they said so.
- Declarative, not imperative: say what is so, not an order.
- Never a way that failed, or a dead end, written as a way that works. Only a way this run saw work.
- Never a claim that a tool, an application or a command does not work or cannot do something.
- Never a failure of this machine, its setup, its network or its connection.
- One rule and one short clause of why. No dates, ids, names of this run's files, this run's figures or quoted logs in the text.
- Not what the context above already says. When it corrects a line you were given, put that line's text, exactly as given after its label, in "replaces".
- Never a password, code, key or any other secret value.
- At most 3. If unsure, propose nothing.

Reply with JSON only: {"proposals": [{"kind": "lesson" or "app_note", "text": "...", "why": "...", "evidence": ["exact quote", "..."], "replaces": "the exact text of a line you were given, or empty"}]}`

// FallbackSystem opens a review made from a job's log, when the transcript itself is no longer kept.
const FallbackSystem = "You are the worker that did the task below, looking back on it. What you have of it is its " +
	"request, the log of what was run and seen, and the report."

// Messages is what the review is sent: the kept transcript as it is, then the instruction. When the transcript ends
// with a user message (a note to the model it never answered), the instruction joins that last message, so no two user
// messages follow each other; every message before it is unchanged either way.
func Messages(k Kept) []llm.Message {
	out := append([]llm.Message(nil), k.Messages...)
	if n := len(out); n > 0 && out[n-1].Role == "user" {
		last := out[n-1]
		last.Content = strings.TrimRight(last.Content, "\n") + "\n\n" + Instruction
		out[n-1] = last
		return out
	}
	return append(out, llm.Message{Role: "user", Content: Instruction})
}

// Sources is what a quote may stand in: the request, and every user message after the opening frame (what commands
// printed, what the person replied). Never the system frame (what earlier tasks kept is not this run's evidence) and
// never the model's own words.
func Sources(k Kept) []string {
	out := []string{k.Request}
	for i, m := range k.Messages {
		if i >= k.PrefixLen && m.Role == "user" {
			out = append(out, m.Content)
		}
	}
	return out
}

// Proposal is one thing the review proposes, as soul read it, with which of its quotes stand in the run.
type Proposal struct {
	Kind     string   `json:"kind"`
	Text     string   `json:"text"`
	Why      string   `json:"why"`
	Evidence []string `json:"evidence"`
	Replaces string   `json:"replaces,omitempty"`
	// InRun says, quote by quote, whether it stands word for word (spacing aside) in what the run read.
	InRun []bool `json:"in_run"`
}

// Decision is soul's reading of the review's answer: the proposals it could read, and why there are none when the
// answer could not be read at all.
type Decision struct {
	Proposals []Proposal `json:"proposals"`
	// Dropped counts proposals left out for their shape (an unknown kind, no evidence, too long).
	Dropped int    `json:"dropped"`
	Error   string `json:"error,omitempty"`
	// Source is what the review read: "transcript", or "job_log" when the transcript was no longer kept.
	Source string `json:"source"`
}

// collapse is s with every run of white space one space, trimmed.
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// Decide reads the review's answer. Anything that is not the shape asked for is left out; an answer with no JSON
// object at all is an error, never a proposal.
func Decide(text string) Decision {
	d := Decision{Proposals: []Proposal{}}
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end < start {
		d.Error = "the answer holds no JSON object"
		return d
	}
	var raw struct {
		Proposals []struct {
			Kind     string   `json:"kind"`
			Text     string   `json:"text"`
			Why      string   `json:"why"`
			Evidence []string `json:"evidence"`
			Replaces string   `json:"replaces"`
		} `json:"proposals"`
	}
	if err := json.Unmarshal([]byte(text[start:end+1]), &raw); err != nil {
		d.Error = "the answer is not the JSON asked for"
		return d
	}
	for _, p := range raw.Proposals {
		kind := strings.TrimSpace(p.Kind)
		body := collapse(p.Text)
		why := collapse(p.Why)
		replaces := collapse(p.Replaces)
		var quotes []string
		for _, q := range p.Evidence {
			if q = strings.TrimSpace(q); q != "" && len([]rune(q)) <= MaxQuoteChars {
				quotes = append(quotes, q)
			}
		}
		if (kind != KindLesson && kind != KindAppNote) || body == "" || len([]rune(body)) > MaxTextChars ||
			len(quotes) == 0 || len(quotes) > MaxEvidence || len(quotes) != len(p.Evidence) ||
			len([]rune(replaces)) > MaxReplaceChar || len(d.Proposals) >= MaxProposals {
			d.Dropped++
			continue
		}
		if r := []rune(why); len(r) > MaxWhyChars {
			why = string(r[:MaxWhyChars])
		}
		d.Proposals = append(d.Proposals, Proposal{Kind: kind, Text: body, Why: why, Evidence: quotes, Replaces: replaces})
	}
	return d
}

// Ground marks, for each proposal's quotes, whether each stands word for word (spacing aside, case kept) in one of
// sources.
func Ground(d *Decision, sources []string) {
	flat := make([]string, 0, len(sources))
	for _, s := range sources {
		if s = collapse(s); s != "" {
			flat = append(flat, s)
		}
	}
	for i := range d.Proposals {
		p := &d.Proposals[i]
		p.InRun = make([]bool, len(p.Evidence))
		for j, q := range p.Evidence {
			q = collapse(q)
			for _, s := range flat {
				if strings.Contains(s, q) {
					p.InRun[j] = true
					break
				}
			}
		}
	}
}
