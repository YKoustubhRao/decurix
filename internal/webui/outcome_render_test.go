package webui

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// maximalDetail is a finished task with every optional field set - tokens, cache, cost, checked - which is what the
// render tests never built, so the trace page shipped crashing on every task that reported a check (W-02, W-15).
func maximalDetail(checked *bool) tasks.Detail {
	now := time.Now().UTC()
	cost := 0.42
	in, out, cached := int64(1200), int64(300), int64(800)
	task := tasks.Task{ID: "t0123456789abcdef", WorkerID: "box", WorkerName: "box", Engine: "thebot", Request: "file the return",
		Type: tasks.TypeOther, Status: tasks.StatusDone, CreatedAt: now, Checked: checked}
	task.Outcome = tasks.DisplayOutcome(task.Status, checked)
	execution := tasks.Execution{ID: "x1", TaskID: task.ID, Attempt: 1, WorkerID: "box", Engine: "thebot", Model: "m",
		Status: tasks.StatusDone, FinalReply: "FINAL-REPORT-MARK", StartedAt: now, EndedAt: now.Add(time.Minute),
		CostUSD: &cost, InputTokens: &in, OutputTokens: &out, CachedInputTokens: &cached, Checked: checked,
		Steps:         []tasks.Step{{ID: "s1", Text: "$ ls", CreatedAt: now}},
		Interventions: []tasks.Intervention{},
	}
	execution.Outcome = tasks.DisplayOutcome(execution.Status, checked)
	return tasks.Detail{Task: task, Worker: tasks.Worker{ID: "box", Name: "box"}, Executions: []tasks.Execution{execution}}
}

// A done task is shown as checked only when its final check judged it, on the task page, the trace and the list -
// and the trace page renders whole whatever the check said.
func TestEveryTaskPageShowsWhetherTheResultWasChecked(t *testing.T) {
	renderer := NewRenderer()
	yes, no := true, false

	for _, tc := range []struct {
		name    string
		checked *bool
		label   string
		check   string
	}{
		{"checked", &yes, ">Done<", "Final check: judged"},
		{"not checked", &no, ">Done, not checked<", "Final check: not run"},
		{"not said", nil, ">Done, not checked<", "Final check: not reported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			detail := maximalDetail(tc.checked)

			var trace bytes.Buffer
			if err := renderer.RenderTaskTrace(&trace, TaskTracePageData{Task: detail}); err != nil {
				t.Fatalf("RenderTaskTrace: %v", err)
			}
			for _, want := range []string{"FINAL-REPORT-MARK", tc.check, tc.label} {
				if !strings.Contains(trace.String(), want) {
					t.Errorf("trace is missing %q", want)
				}
			}
			if strings.Contains(trace.String(), "internal server error") {
				t.Error("trace ended in an error")
			}

			var page bytes.Buffer
			if err := renderer.RenderTask(&page, TaskPageData{Task: detail}); err != nil {
				t.Fatalf("RenderTask: %v", err)
			}
			if !strings.Contains(page.String(), tc.label) {
				t.Errorf("task page is missing %q", tc.label)
			}
			if tc.checked == nil || !*tc.checked {
				if !strings.Contains(page.String(), "Answer, not checked") {
					t.Error("task page calls an unchecked reply plainly the answer")
				}
			}

			var list bytes.Buffer
			summary := tasks.Summary{Task: detail.Task, Executions: 1}
			if err := renderer.RenderTasks(&list, TasksPageData{Window: WindowOption{Value: "7", Phrase: "this week"}, Report: tasks.Report{Totals: tasks.Group{Tasks: 1}}, Tasks: []tasks.Summary{summary}}); err != nil {
				t.Fatalf("RenderTasks: %v", err)
			}
			if !strings.Contains(list.String(), tc.label) {
				t.Errorf("task list is missing %q", tc.label)
			}

			text := TraceText(detail)
			if !strings.Contains(text, tc.check) || !strings.Contains(text, "Status: "+strings.Trim(tc.label, "<>")) {
				t.Errorf("plain-text trace lacks the check or the status:\n%s", text)
			}
		})
	}
}
