package runs

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// R1: a retry started the moment attempt 1 ended. Each trajectory has only its own steps, ledger records and calls,
// though attempt 1's minute of grace after its end runs into attempt 2.
func TestABackToBackRetryKeepsEachAttemptsOwn(t *testing.T) {
	sc := hostedScenario() // attempt 2: starts at t0
	rec := ".decurix/recordings/task-t_h/"
	at1 := t0.Add(-2 * time.Minute)
	old := jsonLines(map[string]any{"i": 0, "t": at1.Sub(t0).Seconds(), "at": at1.Format("2006-01-02T15:04:05-0700"),
		"id": "20261104T100011-0a0b0c0d", "kind": "type", "turn": "j41:1", "element": map[string]any{"name": "Total"},
		"text": "5800.00", "outcome": "failed", "actor": "agent"})
	sc.home.files[rec+"events.jsonl"] = append(old, sc.home.files[rec+"events.jsonl"]...)
	cmd := traceCommand(0, "j41:1", "gui type e1 '5800.00'", "allowed", 1, 500)
	cmd["at"] = at1.Add(time.Second).Format(time.RFC3339Nano)
	sc.home.files[".decurix/ai/traces/t_h.jsonl"] = append(jsonLines(cmd), sc.home.files[".decurix/ai/traces/t_h.jsonl"]...)
	sc.home.files[".decurix/computer/evidence/t_h.records.json"] = []byte(`[{"key":"INV-0041","source":"bill.pdf",
		"fields":{"Total":{"intent":"5,800.00"}},"declared_at":` + fmt.Sprint(at1.Unix()) + `,"saved_at":null,"readback":{}},` +
		strings.TrimPrefix(string(sc.home.files[".decurix/computer/evidence/t_h.records.json"]), "["))
	oldCall := row("c0", -125, "eve", "j41:1", "operator.turn")
	sc.calls.rows = append([]CallRow{oldCall}, sc.calls.rows...)
	attempt1 := tasks.Execution{ID: "e_h0", TaskID: "t_h", Attempt: 1, WorkerID: "eve", Status: tasks.StatusError,
		StartedAt: t0.Add(-5 * time.Minute), EndedAt: t0}
	sc.tasks.detail.Executions[0].Attempt = 2
	sc.tasks.detail.Executions = append([]tasks.Execution{attempt1}, sc.tasks.detail.Executions...)

	second := sc.build(t)
	for _, s := range second.Steps {
		if s.Turn == "j41:1" {
			t.Fatalf("attempt 2 holds attempt 1's step")
		}
	}
	for _, c := range second.Calls {
		if c.ID == "c0" {
			t.Fatal("attempt 2 holds attempt 1's call")
		}
	}
	if len(second.Steps) != 3 || len(second.Calls) != 5 || len(second.Ledger) != 1 || second.Ledger[0].Key != "INV-0042" {
		t.Fatalf("attempt 2: %d steps, %d calls, ledger %+v", len(second.Steps), len(second.Calls), second.Ledger)
	}

	p, blobs := newPuller(t, sc.home)
	if _, err := p.Pull(context.Background(), ExecutionRef{ExecID: "e_h0", TaskID: "t_h", Worker: "eve"}); err != nil {
		t.Fatal(err)
	}
	first, err := Build(context.Background(), Sources{Tasks: sc.tasks, Calls: sc.calls, Store: p.Store, Blobs: blobs}, "e_h0")
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Steps) != 1 || first.Steps[0].Turn != "j41:1" {
		t.Fatalf("attempt 1's steps: %+v", first.Steps)
	}
	if len(first.Calls) != 1 || first.Calls[0].ID != "c0" {
		t.Fatalf("attempt 1's calls: %+v", first.Calls)
	}
	if len(first.Ledger) != 1 || first.Ledger[0].Key != "INV-0041" {
		t.Fatalf("attempt 1's ledger: %+v", first.Ledger)
	}
}

func TestWindowOf(t *testing.T) {
	a := tasks.Execution{ID: "a", Attempt: 1, StartedAt: t0, EndedAt: t0.Add(time.Minute)}
	b := tasks.Execution{ID: "b", Attempt: 2, StartedAt: t0.Add(90 * time.Second), EndedAt: t0.Add(3 * time.Minute)}
	c := tasks.Execution{ID: "c", Attempt: 3, StartedAt: t0.Add(10 * time.Minute)} // still running
	all := []tasks.Execution{c, a, b}
	cases := []struct {
		e        tasks.Execution
		from, to time.Time
	}{
		{a, t0.Add(-windowSlack), b.StartedAt},           // its minute of grace stops at b's start
		{b, b.StartedAt, b.EndedAt.Add(callWindowAfter)}, // starts where a stops, not 2 s before
		{c, c.StartedAt.Add(-windowSlack), time.Time{}},  // running: open
	}
	for _, tc := range cases {
		w := windowOf(tc.e, all)
		if !w.from.Equal(tc.from) || !w.to.Equal(tc.to) {
			t.Errorf("%s: window %v..%v, want %v..%v", tc.e.ID, w.from, w.to, tc.from, tc.to)
		}
	}
	if w := windowOf(tasks.Execution{ID: "x", StartedAt: t0}, nil); !w.from.Equal(t0.Add(-windowSlack)) || !w.to.IsZero() {
		t.Errorf("an execution alone and running: %v..%v", w.from, w.to)
	}
	w := windowOf(a, all)
	if w.holds(b.StartedAt) || !w.holds(b.StartedAt.Add(-time.Nanosecond)) || w.holds(time.Time{}) {
		t.Fatal("the window's ends are not as said")
	}
}
