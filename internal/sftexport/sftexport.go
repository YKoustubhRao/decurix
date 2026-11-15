// Package sftexport turns run records (decurix.trajectory/v1) into a training set: one JSON line per run whose final
// verdict is a checked done, holding the request and then the model's own turns and the command results in the
// worker's protocol (COMMAND:, DONE:, HELP:), or, as a variant, in the tool-call shape open models are tuned on
// (<tool_call>{json}</tool_call> and <tool_response>{json}</tool_response>).
//
// What it keeps and leaves out:
//   - Only runs labelled done whose verdict rests on a check that is not a model's (code, ledger or person basis), or
//     that a person marked correct; never one a person marked wrong or an eval found a false done.
//   - Only steps whose own label is ok: a failed, refused, corrected, overridden or unlabelled step is left out with
//     its result, and counted.
//   - Never a summary: the middle turns are the stored ones, in order. A command's output is the part soul's trace
//     keeps (its first 2 KB), and a turn says when it was cut there.
//   - Secrets were withheld where the text was written; every string passes the control plane's backstops again
//     (withhold.Text, withhold.Known) before it is written.
//
// It reads internal/runs and internal/withhold and imports nothing of httpapi.
package sftexport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/runs"
	"github.com/YKoustubhRao/decurix/internal/withhold"
)

// Schema names one exported line.
const Schema = "decurix.sft/v1"

// Formats of an export.
const (
	// FormatDecurix is the worker's own protocol: role/content messages, COMMAND:/DONE:/HELP: markers.
	FormatDecurix = "decurix"
	// FormatToolCall is the tool-call variant: from/value turns, <tool_call> and <tool_response> blocks.
	FormatToolCall = "toolcall"
)

// outputHeadBytes is how much of a command's output soul's trace keeps (trace.OutputHead in soul).
const outputHeadBytes = 2 << 10

// SystemPrompt opens every record in the worker's protocol.
const SystemPrompt = "Work on the task you are given on this computer by running shell commands, one at a time. " +
	"Every reply is exactly one of these, marker first:\n\nCOMMAND:\n<one shell command>\n\nDONE:\n<your final answer " +
	"for the human, based on output you saw>\n\nHELP:\n<the one thing you need from the human>"

// ToolCallSystemPrompt opens every record in the tool-call variant.
const ToolCallSystemPrompt = "Work on the task you are given on this computer. You have one tool, shell: " +
	`{"name": "shell", "arguments": {"command": "<one shell command>"}}. Call it inside <tool_call></tool_call>; ` +
	"its result comes back inside <tool_response></tool_response>. When the task is finished, answer the human " +
	"with what you saw; when you need the human, ask them."

// Message is one turn of the worker's protocol.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Turn is one turn of the tool-call variant: from is system, human, gpt or tool.
type Turn struct {
	From  string `json:"from"`
	Value string `json:"value"`
}

// Record is one exported run.
type Record struct {
	Schema   string    `json:"schema"`
	Format   string    `json:"format"`
	RunID    string    `json:"run_id"`
	TaskID   string    `json:"task_id"`
	WorkerID string    `json:"worker_id"`
	Messages []Message `json:"messages,omitempty"`
	// Conversations is the tool-call variant's turns.
	Conversations []Turn `json:"conversations,omitempty"`
	Labels        Labels `json:"labels"`
	// Dropped is how many steps were left out for their own label (not ok), and Unshown how many had no command
	// (an action no command of the worker's made).
	Dropped int `json:"dropped_steps"`
	Unshown int `json:"steps_without_command"`
}

// Labels says why the run was taken.
type Labels struct {
	Run           string   `json:"run"`
	Basis         []string `json:"basis"`
	By            string   `json:"by"`
	PersonVerdict string   `json:"person_verdict"`
}

// verdictDoc is the part of decurix.verdict/v1 the export reads.
type verdictDoc struct {
	Verdict string   `json:"verdict"`
	By      string   `json:"by"`
	Basis   []string `json:"basis"`
}

// evalLabel is the part of labels.eval the export reads.
type evalLabel struct {
	FalseDone bool `json:"false_done"`
	Invalid   bool `json:"invalid"`
	Infra     bool `json:"infra"`
}

// Eligible says whether a run belongs in a training set, and why not when it does not: its run label is done, a
// check that is not a model's settled it (code, ledger or person) or a person marked it correct, no person marked it
// wrong, an eval did not find it a false done, and its bundle was kept.
func Eligible(t runs.Trajectory) (bool, string) {
	if t.Labels.Run != "done" {
		return false, "not a checked done"
	}
	if t.Labels.PersonVerdict == "wrong" {
		return false, "a person marked it wrong"
	}
	var ev evalLabel
	if len(t.Labels.Eval) > 0 && string(t.Labels.Eval) != "null" {
		if json.Unmarshal(t.Labels.Eval, &ev) != nil || ev.FalseDone || ev.Invalid || ev.Infra {
			return false, "its eval row says it is not a clean done"
		}
	}
	var v verdictDoc
	if len(t.Verdict) == 0 || json.Unmarshal(t.Verdict, &v) != nil || v.Verdict != "done" {
		return false, "not a checked done"
	}
	checked := t.Labels.PersonVerdict == "correct"
	for _, b := range v.Basis {
		checked = checked || b == "code" || b == "ledger" || b == "person"
	}
	if !checked {
		return false, "checked by a model only"
	}
	for _, m := range t.Integrity.Missing {
		if strings.HasPrefix(m, "no bundle") {
			return false, "no run bundle"
		}
	}
	return true, ""
}

// safe is text with the control plane's secret backstops applied once more.
func safe(text string) string {
	return withhold.Known(withhold.Text(text))
}

// event is one turn pair of a run, in time order: a command and its result, or a question and its answer.
type event struct {
	at      string
	step    *runs.Step
	ask     *runs.Question
	ordinal int
}

// resultText is a command's result as the worker is shown it (operator.go's format), with a note when the record cut
// the output.
func resultText(c *runs.Command) string {
	out := c.OutputHead
	if len(out) >= outputHeadBytes-4 {
		out += "\n[the run record keeps only the first 2 KB of this output]"
	}
	if c.Exit == nil {
		return "Command execution result:\n\noutput:\n" + out
	}
	return fmt.Sprintf("Command execution result:\n\nexit_code: %d\n\noutput:\n%s", *c.Exit, out)
}

// Build makes one run's record in format from its trajectory and its final report (the execution's final reply, as
// the task records keep it). It fails for a run Eligible refuses, or one with no final report.
func Build(t runs.Trajectory, finalReply, format string) (Record, error) {
	if ok, why := Eligible(t); !ok {
		return Record{}, fmt.Errorf("sftexport: %s", why)
	}
	if strings.TrimSpace(finalReply) == "" {
		return Record{}, fmt.Errorf("sftexport: no final report")
	}
	if format == "" {
		format = FormatDecurix
	}
	if format != FormatDecurix && format != FormatToolCall {
		return Record{}, fmt.Errorf("sftexport: unknown format %q", format)
	}
	var v verdictDoc
	_ = json.Unmarshal(t.Verdict, &v)
	rec := Record{Schema: Schema, Format: format, RunID: t.RunID, TaskID: t.TaskID, WorkerID: t.Worker.ID,
		Labels: Labels{Run: t.Labels.Run, Basis: v.Basis, By: v.By, PersonVerdict: t.Labels.PersonVerdict}}
	if rec.Labels.Basis == nil {
		rec.Labels.Basis = []string{}
	}

	var events []event
	for i := range t.Steps {
		s := &t.Steps[i]
		if s.Command == nil {
			rec.Unshown++
			continue
		}
		if s.Label != "ok" {
			rec.Dropped++
			continue
		}
		events = append(events, event{at: s.At, step: s, ordinal: len(events)})
	}
	for i := range t.Questions {
		q := &t.Questions[i]
		if q.Answer == nil {
			continue
		}
		events = append(events, event{at: q.AskedAt, ask: q, ordinal: len(events)})
	}
	sort.SliceStable(events, func(a, b int) bool {
		ta, ea := time.Parse(time.RFC3339Nano, events[a].at)
		tb, eb := time.Parse(time.RFC3339Nano, events[b].at)
		if ea != nil || eb != nil || ta.Equal(tb) {
			return events[a].ordinal < events[b].ordinal
		}
		return ta.Before(tb)
	})

	if format == FormatDecurix {
		rec.Messages = []Message{{Role: "system", Content: SystemPrompt}, {Role: "user", Content: safe(t.Request.Text)}}
		for _, e := range events {
			if e.step != nil {
				rec.Messages = append(rec.Messages,
					Message{Role: "assistant", Content: "COMMAND:\n" + safe(e.step.Command.Text)},
					Message{Role: "user", Content: safe(resultText(e.step.Command))})
				continue
			}
			rec.Messages = append(rec.Messages,
				Message{Role: "assistant", Content: "HELP:\n" + safe(e.ask.Text)},
				Message{Role: "user", Content: "The human replied: " + safe(answerText(e.ask))})
		}
		rec.Messages = append(rec.Messages, Message{Role: "assistant", Content: "DONE:\n" + safe(finalReply)})
		return rec, nil
	}

	rec.Conversations = []Turn{{From: "system", Value: ToolCallSystemPrompt}, {From: "human", Value: safe(t.Request.Text)}}
	for _, e := range events {
		if e.step != nil {
			call, _ := json.Marshal(map[string]any{"name": "shell", "arguments": map[string]string{"command": safe(e.step.Command.Text)}})
			content := map[string]any{"output": safe(e.step.Command.OutputHead)}
			if e.step.Command.Exit != nil {
				content["exit_code"] = *e.step.Command.Exit
			}
			if len(e.step.Command.OutputHead) >= outputHeadBytes-4 {
				content["note"] = "the run record keeps only the first 2 KB of this output"
			}
			resp, _ := json.Marshal(map[string]any{"name": "shell", "content": content})
			rec.Conversations = append(rec.Conversations,
				Turn{From: "gpt", Value: "<tool_call>\n" + string(call) + "\n</tool_call>"},
				Turn{From: "tool", Value: "<tool_response>\n" + string(resp) + "\n</tool_response>"})
			continue
		}
		rec.Conversations = append(rec.Conversations,
			Turn{From: "gpt", Value: safe(e.ask.Text)},
			Turn{From: "human", Value: safe(answerText(e.ask))})
	}
	rec.Conversations = append(rec.Conversations, Turn{From: "gpt", Value: safe(finalReply)})
	return rec, nil
}

// answerText is a person's answer as the records keep it: a secret one only as the withheld mark.
func answerText(q *runs.Question) string {
	if q.Secret {
		return withhold.Secret
	}
	return q.Answer.Text
}

// Source is what Write reads: the runs that ended since a time, each run's record, and its final report.
type Source interface {
	// Ended lists the runs (execution id, task id, worker) that ended since since, oldest first.
	Ended(since time.Time) ([]runs.ExecutionRef, error)
	Trajectory(ctx context.Context, execID string) (runs.Trajectory, error)
	FinalReply(taskID, execID string) (string, error)
}

// Options narrow an export.
type Options struct {
	Since  time.Time
	Format string
	// Worker, when set, takes only that worker's runs.
	Worker string
	// Max bounds the runs written (0: no bound).
	Max int
}

// Summary is what an export did.
type Summary struct {
	Runs     int            `json:"runs"`
	Exported int            `json:"exported"`
	Skipped  map[string]int `json:"skipped"`
}

// Write writes one JSON line to w for every eligible run that ended since opts.Since, and says what it skipped and
// why. A run whose record cannot be built is skipped and counted, never written in part.
func Write(ctx context.Context, w io.Writer, src Source, opts Options) (Summary, error) {
	sum := Summary{Skipped: map[string]int{}}
	refs, err := src.Ended(opts.Since)
	if err != nil {
		return sum, err
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, ref := range refs {
		if ctx.Err() != nil {
			return sum, ctx.Err()
		}
		if opts.Worker != "" && !strings.EqualFold(ref.Worker, opts.Worker) {
			continue
		}
		sum.Runs++
		t, err := src.Trajectory(ctx, ref.ExecID)
		if err != nil {
			sum.Skipped["its run record could not be built"]++
			continue
		}
		if ok, why := Eligible(t); !ok {
			sum.Skipped[why]++
			continue
		}
		reply, err := src.FinalReply(ref.TaskID, ref.ExecID)
		if err != nil {
			sum.Skipped["its final report could not be read"]++
			continue
		}
		rec, err := Build(t, reply, opts.Format)
		if err != nil {
			sum.Skipped[strings.TrimPrefix(err.Error(), "sftexport: ")]++
			continue
		}
		if err := enc.Encode(rec); err != nil {
			return sum, err
		}
		sum.Exported++
		if opts.Max > 0 && sum.Exported >= opts.Max {
			break
		}
	}
	return sum, nil
}
