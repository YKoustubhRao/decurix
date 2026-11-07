// Tests for the carry_state run mode (a learning curve keeps the desktop's state between a repeat's cases; never the
// default) and for labels.eval, the eval rows read back for a run record by its trajectory or its task.
package eval

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// withCarryState is the suite tar b with "carry_state": true in its suite.json.
func withCarryState(t *testing.T, b []byte) []byte {
	t.Helper()
	files := map[string][]byte{}
	var order []string
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		files[h.Name] = body
		order = append(order, h.Name)
	}
	var suite map[string]any
	if err := json.Unmarshal(files["suite.json"], &suite); err != nil {
		t.Fatal(err)
	}
	suite["carry_state"] = true
	files["suite.json"] = mustJSON(t, suite)
	return tarFrom(t, files, order)
}

// A carry_state run resets the desktop before each repeat's first case only; the default resets before every case
// but the arm's first. The run row and the table say which mode ran.
func TestACarryStateRunClearsNothingBetweenARepeatsCases(t *testing.T) {
	s, sb, _, _, _ := newService(t, diligent)
	run, err := s.Start(context.Background(), MatrixRequest{SuiteTar: withCarryState(t, runnerSuite(t)), Repeats: 2, CarryState: true,
		Arms: []Arm{{Name: "default", Kind: ArmModel}}})
	if err != nil {
		t.Fatal(err)
	}
	run = waitDone(t, s, run.ID)
	if !run.CarryState {
		t.Fatalf("the run row does not say it carried state: %+v", run)
	}
	rows, _ := s.Store.Rows(run.ID)
	if len(rows) != 6 {
		t.Fatalf("%d rows, want 6", len(rows))
	}
	sb.mu.Lock()
	resets := sb.resets
	sb.mu.Unlock()
	if resets != 1 {
		t.Fatalf("resets: %d, want 1 (before the second repeat's first case only)", resets)
	}
	md := Summarise(run, rows).Markdown()
	if !strings.Contains(md, "carried from case to case") {
		t.Fatalf("the table does not say state was carried:\n%s", md)
	}

	// The default, on the same cases: every case but the arm's first starts from the snapshot, and the table says so.
	s2, sb2, _, _, _ := newService(t, diligent)
	run2, err := s2.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 2, Arms: []Arm{{Name: "default", Kind: ArmModel}}})
	if err != nil {
		t.Fatal(err)
	}
	run2 = waitDone(t, s2, run2.ID)
	sb2.mu.Lock()
	resets2 := sb2.resets
	sb2.mu.Unlock()
	if run2.CarryState || resets2 != 5 {
		t.Fatalf("default mode: carry %v, resets %d (want 5)", run2.CarryState, resets2)
	}
	rows2, _ := s2.Store.Rows(run2.ID)
	if md := Summarise(run2, rows2).Markdown(); !strings.Contains(md, "reset to the snapshot before every case") {
		t.Fatalf("the default table does not say each case was reset:\n%s", md)
	}
}

// The run's mode and the suite's must agree: carrying state is never run on a suite of independent cases, and a
// suite written as a sequence is never run as independent cases by leaving the flag out.
func TestCarryStateNeedsTheSuiteAndTheRunToAgree(t *testing.T) {
	s, sb, _, _, _ := newService(t, diligent)
	arms := []Arm{{Name: "default", Kind: ArmModel}}
	var bad BadRequest
	if _, err := s.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 1, Arms: arms, CarryState: true}); !errors.As(err, &bad) {
		t.Fatalf("carry_state on a suite of independent cases: %v", err)
	}
	if _, err := s.Start(context.Background(), MatrixRequest{SuiteTar: withCarryState(t, runnerSuite(t)), Repeats: 1, Arms: arms}); !errors.As(err, &bad) {
		t.Fatalf("a carry_state suite without the run's flag: %v", err)
	}
	sb.mu.Lock()
	made := len(sb.specs)
	sb.mu.Unlock()
	if made != 0 {
		t.Fatalf("a refused run made %d desktops", made)
	}
	// The flag is in the suite's own document, so a suite that says it is another case set (another SHA).
	plain, _ := Load(runnerSuite(t))
	carried, _ := Load(withCarryState(t, runnerSuite(t)))
	if plain.SHA == carried.SHA || !carried.CarryState || plain.CarryState {
		t.Fatalf("suite SHAs %s and %s, carry %v/%v", plain.SHA, carried.SHA, plain.CarryState, carried.CarryState)
	}
}

// labels.eval: a row is found by the trajectory it names, else (its bundle never pulled) by its task; a run no eval
// made has none.
func TestEvalLabelReadsARowBackByTrajectoryOrTask(t *testing.T) {
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "eval.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store, err := NewStore(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.Create(Run{ID: "e_run", CreatedAt: now, StartedAt: now, SuiteName: "s1", SuiteSHA: "sha1", Arms: []Arm{{Name: "default"}},
		Cases: []string{"a", "b"}, Repeats: 1, CarryState: true}); err != nil {
		t.Fatal(err)
	}
	for _, r := range []Row{
		{RunID: "e_run", Arm: "default", Repeat: 1, CaseID: "a", TaskID: "t_a", Observed: "complete", Passed: true, TrajectoryID: "x_a", At: now},
		{RunID: "e_run", Arm: "default", Repeat: 1, CaseID: "b", TaskID: "t_b", Observed: "complete", FalseDone: true, Infra: false, At: now},
	} {
		if err := store.AddRow(r); err != nil {
			t.Fatal(err)
		}
	}
	l, ok, err := store.EvalLabel("x_a", "t_other")
	if err != nil || !ok || l.Case != "a" || !l.Passed || l.Suite != "s1" || l.SuiteSHA != "sha1" || l.RunID != "e_run" || !l.CarryState {
		t.Fatalf("by trajectory: %+v %v %v", l, ok, err)
	}
	l, ok, err = store.EvalLabel("x_b", "t_b")
	if err != nil || !ok || l.Case != "b" || l.Passed || !l.FalseDone {
		t.Fatalf("by task: %+v %v %v", l, ok, err)
	}
	// A row that names another trajectory is that run's, not this task's other attempt's.
	if _, ok, _ := store.EvalLabel("x_other", "t_a"); ok {
		t.Fatal("a task's row that names its trajectory labelled another execution")
	}
	if _, ok, err := store.EvalLabel("x_none", "t_none"); ok || err != nil {
		t.Fatalf("no row: ok %v err %v", ok, err)
	}
}
