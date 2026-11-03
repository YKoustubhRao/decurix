package skills

import (
	"encoding/json"
	"regexp"
	"strings"

	"thebot/internal/llm"
)

// ============================================================
// The plan (docs/learning-loop.md, phase B). Once in a goal, when it first starts changing things, the work is written
// down as a case - the records it covers, every place facts about them are kept, what it will write and what it will
// commit - and, when it commits something that cannot be taken back and no method for this kind of task is known yet,
// put to the person once: is anywhere else facts are kept, is anything to be done otherwise. The answer is worth more
// than the questions it saves later: found live (2026-10-23), a run asked the person fifteen times, most of them about
// a record whose explanation was kept in a place the work never looked.
// ============================================================

const planSystem = `A computer worker is about to start changing things for a task. From the request, what the person said, their rules and what the worker has read so far (below), write the plan of the work:
- "scope": the records the work covers - what kind, and how their set is found (a serial range, the rows of a period, the files of a folder).
- "sources": every place facts about those records may be kept, read so far or not (files, folders, sheets, chats, mail, web apps, portals, the person).
- "outputs": what it will write or enter, and where.
- "commits": the actions that cannot be taken back once done - filing, paying, sending a message, submitting to another system - in order; empty when there are none. Saving or changing files on this computer is not one: it can be put back.
- "ask": one short question for the person, only about what the request and what was read leave open about the scope and the sources (anywhere else facts are kept, which period, which records) - or empty.
Reply with JSON only: {"scope": "...", "sources": ["..."], "outputs": ["..."], "commits": ["..."], "ask": "..."}`

// workPlan is the plan of a goal's work, as the model wrote it, and the person's answer to it.
type workPlan struct {
	Scope      string   `json:"scope"`
	Sources    []string `json:"sources"`
	Outputs    []string `json:"outputs"`
	Commits    []string `json:"commits"`
	Ask        string   `json:"ask"`
	PersonSaid string   `json:"personSaid,omitempty"`
}

// changingToolRe is a command of the worker's tools that changes something: typing or choosing on the screen, writing
// an office file or a draft.
var changingToolRe = regexp.MustCompile(`^\s*(?:gui\s+(?:type|select)|office\s+(?:update|create|email))\b`)

// changesSomething reports whether any part of a command is one of the tools' changing commands.
func changesSomething(command string) bool {
	for _, seg := range shellSegments(command) {
		if changingToolRe.MatchString(seg.Text) {
			return true
		}
	}
	return false
}

// methodMarker opens a learned method in the goal's context (docs/learning-loop.md, phase C): with one, the plan is not
// put to the person again.
const methodMarker = "A METHOD FOR THIS KIND OF TASK"

// hasMethod reports whether the host said the request's plan was agreed already (a stage of a planned run:
// RunContext.PlanAgreed, a flag in the job request), or the frame soul built from the host's context holds a learned
// method for its kind of task. Never from the request's own text: a pasted email holding a marker must not skip the
// plan question (review T-32). The method marker counts only in a system message - the frame - never in the user
// message that carries the request.
func (st *operatorState) hasMethod() bool {
	if st.PlanAgreed {
		return true
	}
	for _, m := range st.Messages[:st.prefixEnd()] {
		if m.Role == "system" && strings.Contains(m.Content, methodMarker) {
			return true
		}
	}
	return false
}

// planHeldNote is what the model is told of the command it sent when the plan was put to the person first.
const planHeldNote = "Not run yet: before anything is changed, the plan of the work was put to the person. Their answer " +
	"comes next."

// planBeforeChange writes the goal's plan, once, the first time a command would change something; it returns the
// question to put to the person when the plan writes or commits something and no method for this kind of task is
// known, else "".
func (o *OperatorSkill) planBeforeChange(rc RunContext, st *operatorState, command string) string {
	if st.Planned || !st.Hosted || rc.Provider == nil || !changesSomething(command) {
		return ""
	}
	st.Planned = true
	facts := llm.Message{Role: "user", Content: st.commitSources() + "\nWHAT THE WORKER HAS RUN SO FAR:\n" +
		tailText(strings.Join(st.Ledger, "\n"), 6000) + "\n\nTHE COMMAND IT IS ABOUT TO RUN:\n" + truncateText(command, 600) + "\n"}
	resp, err := rc.Provider.Chat(rc.Ctx, rc.Model, []llm.Message{{Role: "system", Content: planSystem}, facts},
		llm.Options{TimeoutMs: verifyTimeoutMs, Temperature: reviewTemperature})
	if err != nil {
		rc.Logf("operator", "could not write the plan of the work: %v", err)
		return ""
	}
	text := resp.Text
	if i, j := strings.Index(text, "{"), strings.LastIndex(text, "}"); i >= 0 && j > i {
		text = text[i : j+1]
	}
	var plan workPlan
	if json.Unmarshal([]byte(text), &plan) != nil || strings.TrimSpace(plan.Scope) == "" {
		rc.Logf("operator", "the plan of the work could not be read")
		return ""
	}
	st.Plan = &plan
	rc.Logf("operator", "the plan: %d source(s), %d commit(s)", len(plan.Sources), len(plan.Commits))
	// Put once to the person the first time a kind of task writes or commits anything (with no method for it yet):
	// where facts are kept is what a first run most often gets wrong (found live 2026-10-23: told up front "read the
	// client's chat messages, not just the attachments", the run learned it; without the question it never looked).
	if (len(plan.Commits) == 0 && len(plan.Outputs) == 0) || st.hasMethod() {
		return ""
	}
	st.PlanQuestion = true
	return planQuestion(plan)
}

// commitsIssue is a point decided in code at a report: the plan of the work has steps that need the person's OK (it
// files, sends or pays something) and none of them went through in this goal. Raised once a goal - after it, what the
// worker says decides. Found live (2026-10-24): twice in one request and again in the next, the worker reported the
// work done with the returns prepared and nothing filed; the review raised other points, and the person had to ask
// "are both actually filed?".
func (st *operatorState) commitsIssue() []reviewIssue {
	if st.Plan == nil || len(st.Plan.Commits) == 0 || st.CommitsRun > 0 || st.CommitsRaised {
		return nil
	}
	st.CommitsRaised = true
	return []reviewIssue{{
		What: "the plan of the work has these steps, each with the person's OK: " +
			truncateText(strings.Join(st.Plan.Commits, "; "), 600) + " - and none of them went through in this work",
		Why: "if the request asks for them, the work is not done: do them (each is put to the person for their OK when " +
			"you reach it); if they were done before this work, or the person said not to, say so in the report with " +
			"what shows it",
		Settle: "recheck",
	}}
}

// planQuestion is the plan put to the person, with what it leaves open.
func planQuestion(p workPlan) string {
	var b strings.Builder
	b.WriteString("Before I change anything, this is how I will do it:\n")
	b.WriteString("- Covering: " + truncateText(p.Scope, 400) + "\n")
	if len(p.Sources) > 0 {
		b.WriteString("- From: " + truncateText(strings.Join(p.Sources, "; "), 600) + "\n")
	}
	if len(p.Outputs) > 0 {
		b.WriteString("- Writing: " + truncateText(strings.Join(p.Outputs, "; "), 400) + "\n")
	}
	if len(p.Commits) > 0 {
		b.WriteString("- Then, each with your OK first: " + truncateText(strings.Join(p.Commits, "; then "), 400) + "\n")
	}
	b.WriteString("\n")
	ask := strings.TrimSpace(p.Ask)
	if ask == "" {
		ask = "Is there anywhere else facts about this are kept that I should read, or anything I should do differently?"
	}
	b.WriteString(ask)
	return b.String()
}

// planText is the goal's plan as the reviewer is shown it, or "".
func (st *operatorState) planText() string {
	if st.Plan == nil {
		return ""
	}
	p := st.Plan
	text := "Covering: " + p.Scope + "\nFrom: " + strings.Join(p.Sources, "; ") + "\nWriting: " + strings.Join(p.Outputs, "; ") +
		"\nCommits: " + strings.Join(p.Commits, "; ")
	if p.PersonSaid != "" {
		text += "\nThe person's answer to the plan: " + p.PersonSaid
	}
	return text
}
