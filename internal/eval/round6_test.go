package eval

import (
	"context"
	"sync"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/runs"
)

// A question names a fact only by the case's own refers_to: whole words, any case, every word of one phrase (F-L2-2).
func TestAQuestionNamesAFactByItsRefersTo(t *testing.T) {
	f := Fact{ID: "centre", RefersTo: []string{"centre", "center", "cost code"}}
	for q, want := range map[string]bool{
		"Which cost CENTRE should this bill go to?": true,
		"Which cost-center is it?":                  true,
		"What is the code for its cost?":            true,
		"Is the code right?":                        false, // one word of a two-word phrase
		"Should I use the city centres list?":       false, // whole words only
		AskAnything:                                 false,
		"":                                          false,
	} {
		if got := f.NamedBy(q); got != want {
			t.Errorf("%q: %v, want %v", q, got, want)
		}
	}
}

// The asked check says what decided it: the case's data when the question names none of its facts, the expert's
// choice when it names one.
func TestTheAskedCheckSaysWhatDecidedIt(t *testing.T) {
	c := caseWith(OutcomeComplete, gr(t, `{"kind":"final_text_number","number":"1"}`), gr(t, `{"kind":"asked","facts":["ledger"],"when":"asked"}`))
	c.Expected.Accept = []string{OutcomeAsked}
	ask := func(q, fact string) Check {
		r := Grade(c, Evidence{Observation: ObservedQuery, FinalText: q, Questions: []Asked{{Kind: ObservedQuery, Text: q, Fact: fact}}}, Mode{})
		return r.Checks[1]
	}
	if ch := ask("Which ledger?", "ledger"); ch.Result != CheckPass || ch.Trust != TrustModel {
		t.Fatalf("named and chosen: %+v", ch)
	}
	if ch := ask("Which ledger?", ""); ch.Result != CheckFail || ch.Trust != TrustModel {
		t.Fatalf("named, not chosen: %+v", ch)
	}
	// An expert's match to a question that names nothing the fact refers to never counts, and no model decided it.
	if ch := ask("Anything else?", "ledger"); ch.Result != CheckFail || ch.Trust != TrustOutput {
		t.Fatalf("not named: %+v", ch)
	}
}

// fakeBundles records the pulls the runner asks for, and fails those of the tasks in fail.
type fakeBundles struct {
	mu    sync.Mutex
	refs  []runs.ExecutionRef
	fail  map[string]error
	sb    *fakeSandbox
	reset []int // the sandbox's reset count at each pull
}

func (b *fakeBundles) Pull(_ context.Context, ref runs.ExecutionRef) (runs.Bundle, error) {
	b.sb.mu.Lock()
	resets := b.sb.resets
	b.sb.mu.Unlock()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refs = append(b.refs, ref)
	b.reset = append(b.reset, resets)
	if err := b.fail[ref.TaskID]; err != nil {
		return runs.Bundle{}, err
	}
	return runs.Bundle{ExecID: ref.ExecID}, nil
}

// O-6: a model or oracle case's row names its trajectory (the execution id) once its bundle was pulled off the
// practice desktop, before the next case resets it (round 7, F-R3-1: oracle rows too); the nothing arm pulls nothing;
// a pull that fails names none and says so.
func TestARowNamesItsTrajectoryOnceTheBundleIsPulled(t *testing.T) {
	s, sb, _, _, _ := newService(t, diligent)
	s.Tasks = &evalTasks{}
	b := &fakeBundles{sb: sb, fail: map[string]error{}}
	s.Bundles = b
	run, err := s.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 1,
		Arms: []Arm{{Name: "nothing", Kind: ArmNothing}, {Name: "oracle", Kind: ArmOracle}, {Name: "default", Kind: ArmModel}}})
	if err != nil {
		t.Fatal(err)
	}
	run = waitDone(t, s, run.ID)
	rows, _ := s.Store.Rows(run.ID)
	b.mu.Lock()
	refs, resets := b.refs, b.reset
	b.mu.Unlock()
	sb.mu.Lock()
	containers := map[string]string{"oracle": "c-" + sb.specs[1].RunID, "default": "c-" + sb.specs[2].RunID} // made in arm order
	sb.mu.Unlock()
	model := 0
	for _, r := range rows {
		if r.Arm == "nothing" {
			if r.TrajectoryID != "" {
				t.Fatalf("a calibration row names a trajectory: %+v", r)
			}
			continue
		}
		var ref *runs.ExecutionRef
		for i := range refs {
			if refs[i].TaskID == r.TaskID {
				ref = &refs[i]
			}
		}
		if ref == nil || r.TrajectoryID == "" || r.TrajectoryID != ref.ExecID || ref.Worker != containers[r.Arm] {
			t.Fatalf("%s: trajectory %q, pull %+v", r.CaseID, r.TrajectoryID, ref)
		}
		model++
	}
	if model != 6 || len(refs) != 6 {
		t.Fatalf("model and oracle rows %d, pulls %d", model, len(refs))
	}
	// Each pull came before the next case's reset of the arm's desktop (a new arm's desktop is made, not reset).
	for i := 1; i < len(resets); i++ {
		if refs[i].Worker == refs[i-1].Worker && resets[i] <= resets[i-1] {
			t.Fatalf("resets at each pull: %v", resets)
		}
	}

	// A pull that fails: the row names no trajectory, and says why.
	s2, sb2, _, _, _ := newService(t, diligent)
	s2.Tasks = &evalTasks{}
	b2 := &fakeBundles{sb: sb2, fail: map[string]error{}}
	s2.Bundles = failAll{b2}
	run, err = s2.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 1, Cases: []string{"office"}, Arms: []Arm{{Name: "default", Kind: ArmModel}}})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, s2, run.ID)
	rows, _ = s2.Store.Rows(run.ID)
	if len(rows) != 1 || rows[0].TrajectoryID != "" || !containsString(rows[0].Problems, "The practice desktop held no part of the case's run, so the row names no trajectory.") {
		t.Fatalf("failed pull row: %+v", rows)
	}
}

// failAll answers every pull with "nothing found".
type failAll struct{ *fakeBundles }

func (f failAll) Pull(ctx context.Context, ref runs.ExecutionRef) (runs.Bundle, error) {
	_, _ = f.fakeBundles.Pull(ctx, ref)
	return runs.Bundle{}, runs.ErrNothingFound
}
