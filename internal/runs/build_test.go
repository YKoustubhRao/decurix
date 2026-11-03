package runs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/llmrec"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

var update = flag.Bool("update", false, "rewrite the testdata trajectories from the fixtures")

// fakeTasks is a TaskSource over one task.
type fakeTasks struct {
	detail     tasks.Detail
	judgements []JudgementRow
	execTask   map[string]string
}

func (f *fakeTasks) Get(taskID string) (tasks.Detail, error) {
	if taskID != f.detail.ID {
		return tasks.Detail{}, tasks.ErrNotFound
	}
	return f.detail, nil
}

func (f *fakeTasks) JudgementsForTask(taskID string) ([]JudgementRow, error) {
	return f.judgements, nil
}

// lookupTasks is fakeTasks that can also find an execution's task.
type lookupTasks struct{ *fakeTasks }

func (l lookupTasks) TaskOfExecution(execID string) (string, error) {
	if t, ok := l.execTask[execID]; ok {
		return t, nil
	}
	return "", tasks.ErrNotFound
}

// fakeCalls is a CallSource that answers whatever rows it holds, whichever desktop made them (Build must filter).
type fakeCalls struct{ rows []CallRow }

func (f fakeCalls) CallsForTask(taskID, worker string, from, to time.Time) ([]CallRow, error) {
	var out []CallRow
	for _, r := range f.rows {
		if r.TaskID == taskID {
			out = append(out, r)
		}
	}
	return out, nil
}

var t0 = time.Date(2026, 11, 4, 10, 2, 11, 0, time.UTC)

// at is t0 plus s seconds.
func at(s float64) time.Time { return t0.Add(time.Duration(s * float64(time.Second))) }

func evAt(s float64) string { return at(s).Format("2006-01-02T15:04:05-0700") }

func i64(n int64) *int64 { return &n }

func jsonLines(rows ...map[string]any) []byte {
	var b bytes.Buffer
	for _, r := range rows {
		line, _ := json.Marshal(r)
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

func traceCall(s float64, turn, purpose, model, provider, prompt string) map[string]any {
	return map[string]any{"schema": "decurix.soul-turn/v1", "at": at(s).Format(time.RFC3339Nano), "task": "", "job": "j42",
		"turn": turn, "kind": "call", "purpose": purpose, "model": model, "provider": provider, "prompt_sha256": prompt,
		"messages": 9, "usage": map[string]int{"in": 3000, "cached": 2000, "out": 90}, "ms": 2100, "command": nil}
}

func traceCommand(s float64, turn, text, guard string, exit any, ms int64) map[string]any {
	return map[string]any{"schema": "decurix.soul-turn/v1", "at": at(s).Format(time.RFC3339Nano), "job": "j42", "turn": turn,
		"kind": "command", "command": map[string]any{"text": text, "guard": guard, "exit": exit, "ms": ms,
			"output_sha256": strings.Repeat("a", 64), "output_head": "ok"}}
}

func row(id string, s float64, desktop, turn, purpose string) CallRow {
	return CallRow{ID: id, At: at(s), MS: 2000, Provider: "azure", Deployment: "gpt-6.1-sol", Status: 200,
		DesktopID: desktop, TaskID: "t_h", Turn: turn, Purpose: purpose, Caller: "soul",
		Messages: []string{"m1", "m2"}, PrefixSHA: "prefix-" + id, PromptSHA: "prompt-" + id, ResponseSHA: "resp-" + id,
		InputTokens: i64(30000), CachedInputTokens: i64(28000), OutputTokens: i64(200), Problems: []string{}}
}

// scenario is one run's fixture: its home, its records, its calls, and the execution's phase-2 fields.
type scenario struct {
	home   *fakeHome
	tasks  *fakeTasks
	calls  fakeCalls
	extras func(execView) execView
	ref    ExecutionRef
}

func (sc scenario) build(t *testing.T) Trajectory {
	t.Helper()
	p, blobs := newPuller(t, sc.home)
	if _, err := p.Pull(context.Background(), sc.ref); err != nil {
		t.Fatal(err)
	}
	src := Sources{Tasks: sc.tasks, Calls: sc.calls, Store: p.Store, Blobs: blobs}
	view := func(e tasks.Execution) execView { return sc.extras(viewOf(e)) }
	traj, err := build(context.Background(), src, sc.ref.ExecID, view)
	if err != nil {
		t.Fatal(err)
	}
	return traj
}

// hostedScenario: a hosted brain, done and checked by code and the ledger, with one question a person answered.
func hostedScenario() scenario {
	h := newFakeHome()
	rec := ".decurix/recordings/task-t_h/"
	h.files[rec+"meta.json"] = []byte(`{"schema":"decurix.recording/v1","task_id":"t_h","started_at":` +
		fmt.Sprint(t0.Unix()) + `}`)
	h.files[rec+"events.jsonl"] = jsonLines(
		map[string]any{"i": 0, "t": 51, "at": evAt(51), "id": "20261104T100302-1a2b3c4d", "kind": "type", "turn": "j42:1",
			"element": map[string]any{"role": "textbox", "name": "Total", "path": "form/total"}, "text": "5900.00",
			"backend": "atspi", "outcome": "done", "actor": "agent", "before": "frames/a1.png", "after": "frames/a2.png",
			"change": "Total now 5900.00", "expect": map[string]any{"value": "5900.00"}, "obs": "3f9a0c1e22b4d5e6",
			"window": "Records - Chromium", "url": "http://records.example.test/bills/new"},
		map[string]any{"i": 1, "t": 410, "at": evAt(410), "id": "20261104T100901-2b3c4d5e", "kind": "click", "turn": "j42:4",
			"element": map[string]any{"role": "button", "name": "Save"}, "text": nil, "backend": "atspi",
			"outcome": "done", "actor": "agent", "before": "frames/a2.png", "after": "frames/a3.png",
			"change": "Saved INV-0042", "expect": map[string]any{}, "obs": "4a0b1d2f33c5e6f7",
			"window": "Records - Chromium", "url": "http://records.example.test/bills/new"},
	)
	for _, f := range []string{"a1", "a2", "a3"} {
		h.files[rec+"frames/"+f+".png"] = []byte("png-" + f)
	}
	h.files[rec+"obs/3f9a0c1e22b4d5e6.json"] = []byte(`{"window":"Records - Chromium"}`)
	h.files[".decurix/computer/evidence/t_h.records.json"] = []byte(`[{"key":"INV-0042","source":"bill.pdf",
		"fields":{"Total":{"intent":"5,900.00","from":"source","entered":"5900.00"},"Payee":{"intent":"Acme Supplies","from":"source"}},
		"declared_at":` + fmt.Sprint(at(40).Unix()) + `,"saved_at":` + fmt.Sprint(at(411).Unix()) + `,
		"readback":{"seen":["Total","Payee"],"absent":[],"key_shown":true}}]`)
	h.files[".decurix/computer/evidence/t_h.jsonl"] = []byte(`{"kind":"read","file":"bill.pdf"}` + "\n")
	h.files[".decurix/runs/t_h/run.json"] = []byte(`{"schema":"decurix.run-manifest/v1","task_id":"t_h","desktop_id":"eve",
		"agent_sha":"sha256:agent","soul_sha":"sha256:soul","harness":"thebot","brain":"azure","labelled_by":["final"]}`)
	h.files[".decurix/runs/t_h/LABELLED"] = []byte{}
	h.files[".decurix/ai/traces/t_h.jsonl"] = jsonLines(
		traceCall(45, "j42:1", "operator.turn", "default", "openai", "trace-p1"),
		traceCommand(51.5, "j42:1", `gui type 'textbox "Total"' '5900.00'`, "allowed", 0, 900),
		traceCall(120, "j42:2", "operator.turn", "default", "openai", "trace-p2"),
		traceCommand(130, "j42:2", "office read bill.pdf", "allowed", 0, 1500),
		traceCall(300, "j42:3", "operator.turn", "default", "openai", "trace-p3"),
		traceCall(330, "j42:4", "operator.turn", "default", "openai", "trace-p4"),
		traceCall(340, "j42:4", "review", "default", "openai", "trace-p5"),
		traceCommand(411, "j42:4", `gui click 'button "Save"'`, "allowed", 0, 1800),
	)

	question := tasks.Intervention{ID: "i_q1", ExecutionID: "e_h", Kind: tasks.InterventionQuery, Reason: "query",
		Request: "Which ledger should this bill go to?", Reply: "Purchases", RequestedAt: at(200), RepliedAt: at(280)}
	exec := tasks.Execution{ID: "e_h", TaskID: "t_h", Attempt: 1, WorkerID: "eve", Engine: "thebot", Model: "azure",
		Status: tasks.StatusDone, StartedAt: t0, EndedAt: at(449), Interventions: []tasks.Intervention{question}}
	checked := true
	exec.Checked = &checked
	exec.Outcome = tasks.DisplayOutcome(exec.Status, exec.Checked)
	detail := tasks.Detail{Task: tasks.Task{ID: "t_h", WorkerID: "eve", WorkerName: "eve", Request: "Enter bill.pdf as a bill",
		Status: tasks.StatusDone}, Worker: tasks.Worker{ID: "eve", Name: "eve"}, Executions: []tasks.Execution{exec}}
	answer := JudgementRow{ID: "j_ans1", At: at(280), Actor: tasks.Actor{ID: "local", Roles: []string{"owner"}, Kind: tasks.ActorPerson}, Door: "chat", Kind: tasks.JudgementAnswer,
		WorkerID: "eve", TaskID: "t_h", ExecutionID: "e_h", InterventionID: "i_q1", Text: "Purchases",
		Payload: json.RawMessage(`{}`), Changed: json.RawMessage(`{}`)}
	calls := fakeCalls{rows: []CallRow{
		row("c1", 45, "eve", "j42:1", "operator.turn"),
		row("c2", 120, "eve", "j42:2", "operator.turn"),
		row("c3", 300, "eve", "j42:3", "operator.turn"),
		row("c4", 330, "eve", "j42:4", "operator.turn"),
		row("c5", 340, "eve", "j42:4", "review"),
		row("cx", 335, "mallory", "j42:4", "operator.turn"), // another desktop claiming the task: never joined
	}}
	return scenario{home: h, tasks: &fakeTasks{detail: detail, judgements: []JudgementRow{answer}}, calls: calls,
		ref: ExecutionRef{ExecID: "e_h", TaskID: "t_h", Worker: "eve"},
		extras: func(v execView) execView {
			v.Verdict = json.RawMessage(`{"schema":"decurix.verdict/v1","verdict":"done","by":"verify","why":"the saved record reads back",` +
				`"basis":["code","ledger"],"checks":[{"id":"ledger.read_back","result":"pass","basis":"ledger","detail":"INV-0042: 2 of 2 fields seen","refs":[]}],` +
				`"evidence":["Checked on the screen: INV-0042 saved"]}`)
			v.Origin = tasks.Origin{Kind: tasks.OriginPerson, Ref: "chat", ActorID: "local"}
			v.Replies["i_q1"] = reply{JudgementID: "j_ans1", ReplierID: "local"}
			return v
		}}
}

// localScenario: a local brain the proxy never sees: no call rows, the trace's call lines stand in.
func localScenario() scenario {
	h := newFakeHome()
	rec := ".decurix/recordings/task-t_l/"
	h.files[rec+"meta.json"] = []byte(`{"schema":"decurix.recording/v1","task_id":"t_l"}`)
	h.files[rec+"events.jsonl"] = jsonLines(
		map[string]any{"i": 0, "t": 20, "at": evAt(20), "id": "20261104T100231-9a8b7c6d", "kind": "type", "turn": "j7:1",
			"element": map[string]any{"role": "textbox", "name": "Name"}, "text": "Ada Lovelace", "backend": "atspi",
			"outcome": "done", "actor": "agent", "before": "frames/b1.png", "after": "frames/b2.png",
			"change": "Name now Ada Lovelace", "window": "Users - Chromium", "url": "http://records.example.test/users/new"},
	)
	h.files[rec+"frames/b1.png"] = []byte("png-b1")
	h.files[rec+"frames/b2.png"] = []byte("png-b2")
	h.files[".decurix/runs/t_l/run.json"] = []byte(`{"schema":"decurix.run-manifest/v1","harness":"thebot","brain":"ollama"}`)
	h.files[".decurix/ai/traces/t_l.jsonl"] = jsonLines(
		traceCall(10, "j7:1", "operator.turn", "qwen2.5-coder:14b", "ollama", "local-p1"),
		traceCommand(20.5, "j7:1", `gui type 'textbox "Name"' 'Ada Lovelace'`, "allowed", 0, 700),
		traceCall(40, "j7:2", "operator.turn", "qwen2.5-coder:14b", "ollama", "local-p2"),
	)
	exec := tasks.Execution{ID: "e_l", TaskID: "t_l", Attempt: 1, WorkerID: "olga", Engine: "thebot", Model: "ollama",
		Status: tasks.StatusDone, StartedAt: t0, EndedAt: at(60)}
	exec.Outcome = tasks.DisplayOutcome(exec.Status, nil)
	detail := tasks.Detail{Task: tasks.Task{ID: "t_l", WorkerID: "olga", Request: "Add the user Ada Lovelace",
		Status: tasks.StatusDone}, Worker: tasks.Worker{ID: "olga", Name: "olga"}, Executions: []tasks.Execution{exec}}
	return scenario{home: h, tasks: &fakeTasks{detail: detail}, ref: ExecutionRef{ExecID: "e_l", TaskID: "t_l", Worker: "olga"},
		extras: func(v execView) execView {
			v.Verdict = json.RawMessage(`{"schema":"decurix.verdict/v1","verdict":"unchecked","by":"verify","why":"no check could be made",` +
				`"basis":[],"checks":[{"id":"judge","result":"not_made","basis":"model","detail":"a local brain","refs":[]}],"evidence":[]}`)
			return v
		}}
}

// stoppedScenario: the gate asked before a payment and the person said no; the run stopped there.
func stoppedScenario() scenario {
	h := newFakeHome()
	rec := ".decurix/recordings/task-t_s/"
	h.files[rec+"meta.json"] = []byte(`{"schema":"decurix.recording/v1","task_id":"t_s"}`)
	h.files[rec+"events.jsonl"] = jsonLines(
		map[string]any{"i": 0, "t": 30, "at": evAt(30), "id": "20261104T100241-5f6e7d8c", "kind": "click", "turn": "j9:2",
			"element": map[string]any{"role": "button", "name": "Pay"}, "text": nil, "backend": "atspi",
			"outcome": "needs_approval", "reason": "it moves money", "actor": "agent", "before": "frames/c1.png",
			"window": "Payments - Chromium", "url": "http://records.example.test/payments/new"},
	)
	h.files[rec+"frames/c1.png"] = []byte("png-c1")
	h.files[".decurix/runs/t_s/run.json"] = []byte(`{"schema":"decurix.run-manifest/v1","harness":"thebot","brain":"azure"}`)
	h.files[".decurix/ai/traces/t_s.jsonl"] = jsonLines(
		traceCall(25, "j9:2", "operator.turn", "default", "openai", "s-p1"),
		traceCommand(31, "j9:2", `gui click 'button "Pay"'`, "asked", 3, 1200),
	)
	approval := tasks.Intervention{ID: "i_a1", ExecutionID: "e_s", Kind: tasks.InterventionApproval, Reason: "approval",
		Request: "May I click Pay? It moves money.", Reply: "no", RequestedAt: at(32), RepliedAt: at(90)}
	exec := tasks.Execution{ID: "e_s", TaskID: "t_s", Attempt: 1, WorkerID: "sam", Engine: "thebot", Model: "azure",
		Status: tasks.StatusError, StartedAt: t0, EndedAt: at(100), Interventions: []tasks.Intervention{approval}}
	exec.Outcome = tasks.DisplayOutcome(exec.Status, nil)
	detail := tasks.Detail{Task: tasks.Task{ID: "t_s", WorkerID: "sam", Request: "Pay the payee Acme 1,200.00",
		Status: tasks.StatusError}, Worker: tasks.Worker{ID: "sam", Name: "sam"}, Executions: []tasks.Execution{exec}}
	no := JudgementRow{ID: "j_no1", At: at(90), Actor: tasks.Actor{ID: "u_op", Name: "Priya", Roles: []string{"operator"},
		Kind: tasks.ActorPerson}, Door: "card", Kind: tasks.JudgementApproval, WorkerID: "sam", TaskID: "t_s",
		ExecutionID: "e_s", InterventionID: "i_a1", Text: "no",
		Payload: json.RawMessage(`{"approval_id":"ap_1","outcome":"allowed","decision":"no"}`), Changed: json.RawMessage(`{}`)}
	calls := fakeCalls{rows: []CallRow{row("s1", 25, "sam", "j9:2", "operator.turn")}}
	for i := range calls.rows {
		calls.rows[i].TaskID = "t_s"
	}
	return scenario{home: h, tasks: &fakeTasks{detail: detail, judgements: []JudgementRow{no}}, calls: calls,
		ref: ExecutionRef{ExecID: "e_s", TaskID: "t_s", Worker: "sam"},
		extras: func(v execView) execView {
			v.Verdict = json.RawMessage(`{"schema":"decurix.verdict/v1","verdict":"stopped","by":"worker","why":"the person said no to the payment",` +
				`"basis":["person"],"checks":[],"evidence":[]}`)
			v.Origin = tasks.Origin{Kind: tasks.OriginPerson, Ref: "card", ActorID: "u_op"}
			v.Replies["i_a1"] = reply{JudgementID: "j_no1", ReplierID: "u_op"}
			return v
		}}
}

// golden compares traj with testdata/name, or rewrites it under -update.
func golden(t *testing.T, name string, traj Trajectory) {
	t.Helper()
	got, err := json.MarshalIndent(traj, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/runs/ -run Golden -update)", err)
	}
	if !bytes.Equal(bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n")), got) {
		t.Fatalf("%s differs from what Build makes now (run with -update if the change is intended):\n%s", name, got)
	}
}

func TestGoldenHosted(t *testing.T) {
	traj := hostedScenario().build(t)
	golden(t, "hosted.json", traj)

	if traj.Schema != Schema || traj.RunID != "e_h" || traj.Outcome != tasks.StateDoneChecked {
		t.Fatalf("head = %+v", traj)
	}
	// The other desktop's row is not joined.
	for _, c := range traj.Calls {
		if c.ID == "cx" {
			t.Fatal("a call from another desktop was joined")
		}
	}
	if len(traj.Calls) != 5 || traj.Usage.Calls != 5 || traj.Models[0].Purposes["operator.turn"] != 4 {
		t.Fatalf("calls %d usage %+v models %+v", len(traj.Calls), traj.Usage, traj.Models)
	}
	// Steps: the type (turn 1), the office read with no event (turn 2), the save (turn 4).
	if len(traj.Steps) != 3 {
		t.Fatalf("steps = %+v", traj.Steps)
	}
	typ, read, save := traj.Steps[0], traj.Steps[1], traj.Steps[2]
	if typ.Command == nil || typ.Command.Tool != "gui" || typ.Action == nil || *typ.Call != 0 || typ.PromptSHA256 != "prompt-c1" {
		t.Fatalf("type step = %+v", typ)
	}
	if read.Action != nil || read.Command == nil || read.Command.Tool != "office" || *read.Call != 1 {
		t.Fatalf("a command with no event is not its own step: %+v", read)
	}
	// Two calls in turn 4 (the operator's, then a review): the save points at the last one at or before it.
	if save.Call == nil || traj.Calls[*save.Call].ID != "c5" {
		t.Fatalf("the save points at call %v", save.Call)
	}
	for _, s := range traj.Steps {
		if s.Label != "ok" {
			t.Fatalf("step %d labelled %q in a run done by code and the ledger", s.I, s.Label)
		}
	}
	if q := traj.Questions[0]; q.Answer == nil || q.Answer.JudgementID != "j_ans1" || q.Answer.Actor == nil ||
		q.Answer.Actor.ID != "local" {
		t.Fatalf("question = %+v", q)
	}
	if traj.Ledger[0].Status != "matched" || traj.Ledger[0].Fields[0].Label != "Total" || traj.Ledger[0].Fields[0].AtSave != "5900.00" {
		t.Fatalf("ledger = %+v", traj.Ledger)
	}
	if len(traj.Integrity.Missing) != 2 || traj.Integrity.BundleSHA256 == "" || traj.Integrity.Parts["soul_trace"] == "" {
		t.Fatalf("integrity = %+v", traj.Integrity)
	}
}

func TestGoldenLocal(t *testing.T) {
	traj := localScenario().build(t)
	golden(t, "local.json", traj)
	if len(traj.Calls) != 2 || traj.Calls[0].Source != "soul_trace" || traj.Models[0].Deployment != "qwen2.5-coder:14b" ||
		traj.Models[0].Provider != "ollama" || traj.Usage.InputTokens != 6000 {
		t.Fatalf("a local brain's calls from the trace: %+v / %+v / %+v", traj.Calls, traj.Models, traj.Usage)
	}
	if s := traj.Steps[0]; s.Call == nil || *s.Call != 0 || s.PromptSHA256 != "local-p1" || s.Label != "unlabelled" {
		t.Fatalf("step = %+v", s)
	}
}

func TestGoldenStopped(t *testing.T) {
	traj := stoppedScenario().build(t)
	golden(t, "stopped.json", traj)
	s := traj.Steps[0]
	if s.Gate.Decision != "denied" || s.Gate.JudgementID != "j_no1" || s.Gate.RequestID != "i_a1" || s.Label != "overridden" {
		t.Fatalf("stopped step = %+v", s)
	}
}

func TestBuildWithNoBundle(t *testing.T) {
	sc := hostedScenario()
	s, blobs := newTestStore(t)
	src := Sources{Tasks: lookupTasks{&fakeTasks{detail: sc.tasks.detail, execTask: map[string]string{"e_h": "t_h"}}},
		Calls: sc.calls, Store: s, Blobs: blobs}
	traj, err := Build(context.Background(), src, "e_h")
	if err != nil {
		t.Fatal(err)
	}
	if len(traj.Steps) != 0 || len(traj.Integrity.Missing) != 1 || !strings.HasPrefix(traj.Integrity.Missing[0], "no bundle") {
		t.Fatalf("no-bundle trajectory: steps %d, missing %v", len(traj.Steps), traj.Integrity.Missing)
	}
	if string(traj.Verdict) != "null" {
		t.Fatalf("an execution without a verdict gives %s", traj.Verdict)
	}
	// Without an execution lookup, an execution with no bundle is unknown.
	src.Tasks = sc.tasks
	if _, err := Build(context.Background(), src, "e_h"); !errors.Is(err, ErrUnknownExecution) {
		t.Fatalf("Build = %v, want ErrUnknownExecution", err)
	}
	if _, err := Build(context.Background(), Sources{Tasks: src.Tasks, Store: s}, "e_none"); !errors.Is(err, ErrUnknownExecution) {
		t.Fatalf("Build of an unknown execution = %v", err)
	}
}

func TestAStepBeforeAnyCallHasNone(t *testing.T) {
	calls := []Call{{I: 0, Turn: "j:1", At: at(100).Format(time.RFC3339Nano)}}
	if got := callFor(calls, "j:1", at(50)); got != -1 {
		t.Fatalf("a step before its turn's first call points at %d", got)
	}
	if got := callFor(calls, "", at(150)); got != -1 {
		t.Fatalf("a step with no turn points at %d", got)
	}
	if got := callFor(calls, "j:1", at(150)); got != 0 {
		t.Fatalf("got %d", got)
	}
}

func TestCommandGate(t *testing.T) {
	three, zero := 3, 0
	approval := tasks.Intervention{ID: "i1", Kind: tasks.InterventionApproval, RequestedAt: at(10)}
	e := execView{Execution: tasks.Execution{Interventions: []tasks.Intervention{approval}},
		Replies: map[string]reply{"i1": {JudgementID: "j1"}}}
	yes := map[string]JudgementRow{"j1": {ID: "j1", Payload: json.RawMessage(`{"decision":"yes"}`)}}
	cases := []struct {
		name string
		c    *traceCmd
		js   map[string]JudgementRow
		want string
	}{
		{"allowed", &traceCmd{Guard: "allowed", Exit: &zero}, nil, "not_needed"},
		{"ran after a yes", &traceCmd{Guard: "asked", Exit: &zero}, nil, "granted"},
		{"paused, then a yes", &traceCmd{Guard: "asked"}, yes, "granted"},
		{"paused, no answer yet", &traceCmd{Guard: "asked"}, nil, "asked"},
		{"the gate held what it did, then a yes", &traceCmd{Guard: "asked", Exit: &three}, yes, "granted"},
		{"refused", &traceCmd{Guard: "refused"}, nil, "refused"},
	}
	for _, c := range cases {
		if got := gateOf("", c.c, at(9), e, c.js); got.Decision != c.want {
			t.Errorf("%s: gate %q, want %q", c.name, got.Decision, c.want)
		}
	}
}

func TestStepLabels(t *testing.T) {
	done := json.RawMessage(`{"verdict":"done","basis":["code"]}`)
	modelOnly := json.RawMessage(`{"verdict":"done","basis":["model","screen"]}`)
	unchecked := json.RawMessage(`{"verdict":"unchecked","basis":[]}`)
	target := json.RawMessage(`{"name":"Total"}`)
	ledger := []LedgerRecord{{Key: "INV-1", Fields: []LedgerField{{Label: "Total", Status: "differs"}}}}
	okLedger := []LedgerRecord{{Key: "INV-1", Fields: []LedgerField{{Label: "Total", Status: "matched"}}}}
	person := tasks.Actor{ID: "u", Kind: tasks.ActorPerson}
	cases := []struct {
		name    string
		step    Step
		ledger  []LedgerRecord
		verdict json.RawMessage
		js      []JudgementRow
		want    string
	}{
		{"agent step, done by code", Step{Action: &Action{Actor: "agent"}}, nil, done, nil, "ok"},
		{"done by a model and the screen only", Step{Action: &Action{Actor: "agent"}}, nil, modelOnly, nil, "unlabelled"},
		{"unchecked run", Step{Action: &Action{Actor: "agent"}}, nil, unchecked, nil, "unlabelled"},
		{"no verdict", Step{}, nil, json.RawMessage("null"), nil, "unlabelled"},
		{"a person acted", Step{Action: &Action{Actor: "human"}}, nil, done, nil, "overridden"},
		{"approval denied", Step{Gate: Gate{Decision: "denied"}}, nil, done, nil, "overridden"},
		{"a correction names the field", Step{At: rfc3339(at(1)), Action: &Action{Target: target}}, okLedger, done,
			[]JudgementRow{{Kind: tasks.JudgementCorrection, Text: "the total is wrong", Actor: person, At: at(5)}}, "corrected"},
		{"a correction names the record", Step{At: rfc3339(at(1)), Action: &Action{Target: target}}, okLedger, done,
			[]JudgementRow{{Kind: tasks.JudgementCorrection, Text: "INV-1 should be another payee", Actor: person, At: at(5)}}, "corrected"},
		{"a field that differs, then a person answered", Step{At: rfc3339(at(1)), Action: &Action{Target: target}}, ledger, done,
			[]JudgementRow{{Kind: tasks.JudgementAnswer, Text: "use 5,900", Actor: person, At: at(5)}}, "corrected"},
		{"a field that differs, answered before the step", Step{At: rfc3339(at(10)), Action: &Action{Target: target}}, ledger, done,
			[]JudgementRow{{Kind: tasks.JudgementAnswer, Text: "use 5,900", Actor: person, At: at(5)}}, "ok"},
	}
	for _, c := range cases {
		steps := []Step{c.step}
		labelSteps(steps, c.ledger, c.verdict, c.js)
		if steps[0].Label != c.want {
			t.Errorf("%s: label %q, want %q", c.name, steps[0].Label, c.want)
		}
	}
}

// The control plane's own stores are the run record's sources.
var (
	_ TaskSource = (*tasks.Store)(nil)
	_ CallSource = (*llmrec.Store)(nil)
)

func TestViewReadsEachRepliesJudgement(t *testing.T) {
	e := tasks.Execution{ID: "e1", Interventions: []tasks.Intervention{
		{ID: "i1", JudgementID: "j_1", ReplierID: "u_1"}, {ID: "i2"}}}
	v := viewOf(e)
	if v.Replies["i1"] != (reply{JudgementID: "j_1", ReplierID: "u_1"}) || v.Replies["i2"] != (reply{}) {
		t.Fatalf("replies = %+v", v.Replies)
	}
	if v.Verdict != nil || v.Origin.Kind != "" {
		t.Fatalf("an execution with no verdict or origin: %+v", v)
	}
}
