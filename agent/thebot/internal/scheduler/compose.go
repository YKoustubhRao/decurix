package scheduler

import (
	"fmt"
	"os"
	"strings"

	"thebot/internal/llm"
	"thebot/internal/skills"
)

// synthesisInstruction closes the final call of a multi-step job. The old
// "final answer" for such a job was every step's raw output under a
// "Subtask N (...)" heading - a transcript, not a reply, and on a Flowera
// desktop the only thing the human reads.
const synthesisInstruction = `Write your reply to the request now, speaking directly to the human.
- Use only what the steps above actually produced. Give the answer itself, not a description of the steps.
- Name any file you wrote or downloaded by its full path. Only quote a file's contents if they are shown above.
- If something did not work, say so plainly and say what is still missing.
- Nothing more will run after this reply, so do not ask for approval or permission.
- Be concise.`

// compose decides a finished job's outcome and writes its reply.
func (e *execution) compose() (Outcome, string) {
	var live []*node
	for _, n := range e.nodes {
		if n.state == StepSkipped && !n.hasResult {
			continue
		}
		if n.superseded {
			continue
		}
		live = append(live, n)
	}

	allDone := len(live) > 0
	for _, n := range live {
		if n.state != StepDone {
			allDone = false
		}
	}

	if len(e.nodes) == 1 {
		n := e.nodes[0]
		if n.state == StepDone {
			return OutcomeAnswered, singleAnswer(n.result)
		}
		return OutcomeFailed, e.failureAnswer(n)
	}

	outcome := OutcomeAnswered
	if !allDone {
		outcome = OutcomeFailed
	}

	if answer := e.synthesize(live); answer != "" {
		return outcome, e.prefixStop(answer)
	}
	return outcome, e.prefixStop(deterministicSummary(live))
}

func (e *execution) prefixStop(answer string) string {
	if e.stopReason == "" {
		return answer
	}
	return fmt.Sprintf("I stopped before finishing - %s.\n\n%s", e.stopReason, answer)
}

// singleAnswer is a one-step job's reply. Most skills already produce one;
// the terminal skill produces a log, which is reformatted rather than sent
// back to a model just to be reworded.
func singleAnswer(r skills.Result) string {
	if r.SkillName != "terminal" {
		return r.Output
	}
	command, _ := r.Meta["command"].(string)
	exit, _ := r.Meta["exitCode"].(int)
	stdout := ""
	if i := strings.Index(r.Output, "\n\nStdout:\n"); i >= 0 {
		stdout = strings.TrimRight(r.Output[i+len("\n\nStdout:\n"):], "\n")
	}
	if strings.TrimSpace(stdout) == "" {
		return fmt.Sprintf("Ran `%s` - it finished with exit code %d and printed nothing.", command, exit)
	}
	return fmt.Sprintf("Ran `%s` (exit code %d):\n\n```\n%s\n```", command, exit, truncate(stdout, 6000))
}

func (e *execution) failureAnswer(n *node) string {
	r := n.result
	var b strings.Builder
	if e.stopReason != "" {
		fmt.Fprintf(&b, "I stopped before finishing - %s.", e.stopReason)
	} else {
		b.WriteString("I couldn't get this done.")
	}
	if reason, _ := r.Meta["failureReasoning"].(string); reason != "" {
		b.WriteString(" " + reason)
	}
	if out := strings.TrimSpace(r.Output); out != "" {
		b.WriteString("\n\n" + truncate(out, 2000))
	}
	return b.String()
}

// synthesize writes a multi-step job's reply with one model call that sees
// every step's result. Returns "" when it cannot, and the caller falls back
// to a plain summary - a job never ends without an answer because its last
// model call failed.
func (e *execution) synthesize(live []*node) string {
	if e.ctx.Err() != nil || e.s.Provider == nil {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "The request was:\n%s\n\nYou worked on it in these steps:\n", e.req.Prompt)
	for _, n := range live {
		status := "done"
		limit := 1800
		switch n.state {
		case StepFailed:
			status, limit = "FAILED", 700
		case StepBlocked:
			status, limit = "not run - a step it needed failed", 0
		}
		fmt.Fprintf(&b, "\nStep %d [%s] %s - %s\n", n.step.ID, n.step.Type, truncate(n.step.Text, 300), status)
		if limit > 0 {
			if out := strings.TrimSpace(n.result.Output); out != "" {
				b.WriteString(truncate(out, limit) + "\n")
			}
		}
		if path, _ := n.result.Meta["path"].(string); path != "" {
			fmt.Fprintf(&b, "(file: %s)\n", path)
			// The file itself, not just its name. Told only "Wrote
			// /tmp/work/go-version.md (12 lines)", the reply quoted a whole
			// note back to the human - one it made up on the spot, which did
			// not match the file on disk.
			if n.step.Type == "file-write" && n.state == StepDone {
				if data, err := os.ReadFile(path); err == nil {
					fmt.Fprintf(&b, "The file now contains:\n%s\n", truncate(string(data), 1500))
				}
			}
		}
	}
	b.WriteString("\n" + synthesisInstruction)

	messages := []llm.Message{e.system}
	messages = append(messages, e.req.History...)
	messages = append(messages, llm.Message{Role: "user", Content: b.String()})

	e.log("scheduler: writing the reply from what the steps found...")
	resp, err := e.s.Provider.Chat(e.ctx, e.s.Models.Default, messages, llm.Options{TimeoutMs: 180000, Temperature: 0.3})
	if err != nil {
		e.log("scheduler: could not write a combined reply (" + err.Error() + ") - summarising the steps instead")
		return ""
	}
	// A reply cannot need approval - it runs nothing. The approval marker the
	// host's rules teach turns up here anyway, and would make the host page
	// a human to approve a summary.
	answer := strings.TrimSpace(resp.Text)
	for _, marker := range []string{"NEEDS_APPROVAL:", "HELP:", "DONE:"} {
		answer = strings.TrimSpace(strings.TrimPrefix(answer, marker))
	}
	return answer
}

func deterministicSummary(live []*node) string {
	var b strings.Builder
	for _, n := range live {
		status := "done"
		switch n.state {
		case StepFailed:
			status = "failed"
		case StepBlocked:
			status = "not run"
		}
		fmt.Fprintf(&b, "**Step %d** - %s (%s)\n", n.step.ID, truncate(n.step.Text, 200), status)
		if out := strings.TrimSpace(n.result.Output); out != "" && n.state != StepBlocked {
			b.WriteString(truncate(out, 1200) + "\n")
		}
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}
