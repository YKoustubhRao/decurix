// Tests for what Build reads besides the bundle's steps: labels.eval from the eval rows (an eval row names its
// trajectory, so the label is read back by execution, or by task for a row whose bundle was never pulled) and
// worker.rules_sha from the run's manifest.
package runs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// fakeEvals is an EvalSource holding one label, keyed by execution and by task.
type fakeEvals struct {
	byExec, byTask map[string]EvalLabel
	err            error
	asked          [][2]string
}

func (f *fakeEvals) EvalLabel(execID, taskID string) (EvalLabel, bool, error) {
	f.asked = append(f.asked, [2]string{execID, taskID})
	if f.err != nil {
		return EvalLabel{}, false, f.err
	}
	if l, ok := f.byExec[execID]; ok {
		return l, true, nil
	}
	l, ok := f.byTask[taskID]
	return l, ok, nil
}

// buildWith builds the hosted scenario's run with evals as its eval source.
func buildWith(t *testing.T, evals EvalSource) Trajectory {
	t.Helper()
	sc := hostedScenario()
	p, blobs := newPuller(t, sc.home)
	if _, err := p.Pull(context.Background(), sc.ref); err != nil {
		t.Fatal(err)
	}
	traj, err := Build(context.Background(), Sources{Tasks: sc.tasks, Calls: sc.calls, Store: p.Store, Blobs: blobs, Evals: evals}, sc.ref.ExecID)
	if err != nil {
		t.Fatal(err)
	}
	return traj
}

// A run an eval case made carries the case's grade: suite, case, arm, repeat, passed, false done.
func TestLabelsEvalIsReadFromTheEvalRows(t *testing.T) {
	evals := &fakeEvals{byExec: map[string]EvalLabel{"e_h": {RunID: "e_run1", Suite: "s", SuiteSHA: "abc", Case: "c1", Arm: "default",
		Repeat: 2, Observed: "final", Passed: true}}}
	traj := buildWith(t, evals)
	var got EvalLabel
	if err := json.Unmarshal(traj.Labels.Eval, &got); err != nil {
		t.Fatalf("labels.eval %s: %v", traj.Labels.Eval, err)
	}
	if got.Case != "c1" || got.Arm != "default" || got.Repeat != 2 || !got.Passed || got.FalseDone || got.Suite != "s" {
		t.Fatalf("labels.eval = %+v", got)
	}
	if len(evals.asked) != 1 || evals.asked[0] != [2]string{"e_h", "t_h"} {
		t.Fatalf("the eval rows were asked %v, want the execution and its task", evals.asked)
	}
	var doc map[string]any
	b, _ := json.Marshal(traj)
	_ = json.Unmarshal(b, &doc)
	eval := doc["labels"].(map[string]any)["eval"].(map[string]any)
	for _, k := range []string{"suite", "case", "arm", "repeat", "passed", "false_done"} {
		if _, ok := eval[k]; !ok {
			t.Errorf("labels.eval has no %q: %v", k, eval)
		}
	}
}

// No eval made the run, or no eval store is here: labels.eval stays null. A read that fails is said, and the run's
// record is still built.
func TestLabelsEvalIsNullWithoutARowAndSaysAFailedRead(t *testing.T) {
	if traj := buildWith(t, nil); string(traj.Labels.Eval) != "null" {
		t.Fatalf("no eval source: labels.eval = %s", traj.Labels.Eval)
	}
	if traj := buildWith(t, &fakeEvals{}); string(traj.Labels.Eval) != "null" {
		t.Fatalf("no eval row: labels.eval = %s", traj.Labels.Eval)
	}
	traj := buildWith(t, &fakeEvals{err: errors.New("disk")})
	if string(traj.Labels.Eval) != "null" {
		t.Fatalf("a failed read: labels.eval = %s", traj.Labels.Eval)
	}
	said := false
	for _, m := range traj.Integrity.Missing {
		said = said || strings.HasPrefix(m, "labels.eval:")
		if strings.Contains(m, "disk") {
			t.Fatalf("the error's own text reached the record: %q", m)
		}
	}
	if !said {
		t.Fatalf("a failed read of the eval rows is not said: %v", traj.Integrity.Missing)
	}
}

// worker.rules_sha is what the desktop's manifest says the run's rules hashed to.
func TestRulesSHAComesFromTheManifest(t *testing.T) {
	sc := hostedScenario()
	sc.home.files[".decurix/runs/t_h/run.json"] = []byte(`{"schema":"decurix.run-manifest/v1","task_id":"t_h","desktop_id":"eve",
		"agent_sha":"sha256:agent","soul_sha":"sha256:soul","rules_sha":"sha256:rules","harness":"thebot","brain":"azure","labelled_by":["final"]}`)
	traj := sc.build(t)
	if traj.Worker.RulesSHA != "sha256:rules" {
		t.Fatalf("worker.rules_sha = %q", traj.Worker.RulesSHA)
	}
}
