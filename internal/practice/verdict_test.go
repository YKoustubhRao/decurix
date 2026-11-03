package practice

import (
	"context"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// A practice case's task record carries the checker's grade as a verdict (by practice), and checked is derived from
// it: never a bare checked (H-CP-2).
func TestAPracticeGradeIsRecordedAsAVerdict(t *testing.T) {
	s, _, _, sb, tk := service(t)
	sb.act["varied-1"] = behaviour{kind: "final", text: "All done!", outputs: map[string]string{"out.txt": "total 999"}}
	su := approved(t, s)
	run, err := s.StartRun(context.Background(), su.ID, su.Source.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	done := waitRun(t, s, run.ID)
	tk.mu.Lock()
	defer tk.mu.Unlock()
	if len(tk.outcomes) == 0 {
		t.Fatal("no outcome recorded")
	}
	for _, f := range done.Findings {
		o, ok := tk.outcomes[f.TaskID]
		if !ok {
			t.Fatalf("no outcome for %s", f.CaseID)
		}
		if o.Checked != nil {
			t.Fatalf("%s: a bare checked was recorded", f.CaseID)
		}
		v := o.Verdict
		if v == nil || v.By != tasks.ByPractice || len(v.Checks) != 1 || v.Checks[0].Basis != "code" {
			t.Fatalf("%s: verdict %+v", f.CaseID, v)
		}
		want := tasks.GateNotDone
		if f.Passed {
			want = tasks.GateDone
		}
		if v.Verdict != want {
			t.Fatalf("%s passed=%v, verdict %s", f.CaseID, f.Passed, v.Verdict)
		}
	}
}

func TestAGradeThatCouldNotBeMadeIsUnchecked(t *testing.T) {
	if v := GraderVerdict(tasks.ByEval, true, false, "eval.graders", "x", "case:c"); v.Verdict != tasks.GateUnchecked || v.Checks[0].Result != "not_made" {
		t.Fatalf("ungraded: %+v", v)
	}
	if v := GraderVerdict(tasks.ByEval, true, true, "eval.graders", "x", "case:c"); v.Verdict != tasks.GateDone || v.By != tasks.ByEval {
		t.Fatalf("passed: %+v", v)
	}
}
