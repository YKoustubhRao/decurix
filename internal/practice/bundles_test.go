// Tests that a Simulate case's run bundle is pulled off the practice desktop before the next case's reset wipes it,
// as an eval case's is, so the case's task has a trajectory with steps; a pull that fails is said in the finding.
package practice

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/runs"
)

// pullLog is a Bundles that notes each pull in the sandbox's log (so its place among the resets shows), and fails
// with fail when set.
type pullLog struct {
	mu   sync.Mutex
	sb   *fakeSandbox
	refs []runs.ExecutionRef
	fail error
}

func (p *pullLog) Pull(_ context.Context, ref runs.ExecutionRef) (runs.Bundle, error) {
	p.mu.Lock()
	p.refs = append(p.refs, ref)
	p.mu.Unlock()
	p.sb.note("pull " + ref.TaskID)
	if p.fail != nil {
		return runs.Bundle{}, p.fail
	}
	return runs.Bundle{ExecID: ref.ExecID}, nil
}

func TestEachSimulateCaseBundleIsPulledBeforeTheNextReset(t *testing.T) {
	s, _, _, sb, tk := service(t)
	pulls := &pullLog{sb: sb}
	s.Bundles = pulls
	su := approved(t, s)
	run, err := s.StartRun(context.Background(), su.ID, su.Source.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	done := waitRun(t, s, run.ID)
	if done.Status != "passed" {
		t.Fatalf("run %+v", done)
	}
	pulls.mu.Lock()
	refs := append([]runs.ExecutionRef{}, pulls.refs...)
	pulls.mu.Unlock()
	tk.mu.Lock()
	opened := append([]string{}, tk.opened...)
	tk.mu.Unlock()
	if len(refs) != 5 || len(opened) != 5 {
		t.Fatalf("pulls %d for %d cases", len(refs), len(opened))
	}
	for i, ref := range refs {
		if ref.TaskID != opened[i] || ref.ExecID == "" || ref.Worker != "desk" {
			t.Fatalf("pull %d: %+v, want the case's task %s on the practice container", i, ref, opened[i])
		}
	}
	// Every pull comes before the next case's reset: the log reads pull, reset, ..., pull.
	sb.mu.Lock()
	var order []string
	for _, l := range sb.log {
		if l == "reset" || strings.HasPrefix(l, "pull ") {
			order = append(order, strings.Fields(l)[0])
		}
	}
	sb.mu.Unlock()
	if got := strings.Join(order, ","); got != "pull,reset,pull,reset,pull,reset,pull,reset,pull" {
		t.Fatalf("pulls and resets: %s", got)
	}
	for _, f := range done.Findings {
		for _, p := range f.Problems {
			if strings.Contains(p, "bundle") {
				t.Fatalf("a pulled case says its bundle was not: %q", p)
			}
		}
	}
}

// A pull that fails leaves the case's checks as they were and says so in its finding.
func TestAFailedSimulatePullIsSaidAndFailsNothing(t *testing.T) {
	s, _, _, sb, _ := service(t)
	s.Bundles = &pullLog{sb: sb, fail: errors.New("docker gone")}
	su := approved(t, s)
	run, err := s.StartRun(context.Background(), su.ID, su.Source.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	done := waitRun(t, s, run.ID)
	if done.Status != "passed" {
		t.Fatalf("a failed pull changed the run's result: %+v", done)
	}
	for _, f := range done.Findings {
		said := false
		for _, p := range f.Problems {
			said = said || strings.Contains(p, "could not be pulled")
			if strings.Contains(p, "docker gone") {
				t.Fatalf("the error's own text reached the finding: %q", p)
			}
		}
		if !said {
			t.Fatalf("%s: a failed pull is not said: %v", f.CaseID, f.Problems)
		}
	}
}
