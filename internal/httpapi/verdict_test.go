package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// finalWithVerdict posts a final callback for a task on box with a verdict and checked flag, as a desktop does.
func finalWithVerdict(t *testing.T, s *Server, taskID, reply, verdict string, checked bool) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"desktop_id": "box", "vm_name": "box", "kind": "final", "content": reply, "task_id": taskID, "checked": checked,
		"verdict": json.RawMessage(verdict),
	})
	req := httptest.NewRequest(http.MethodPost, "/api/querytext", strings.NewReader(string(body)))
	req = req.WithContext(context.WithValue(req.Context(), callerKey{}, "box"))
	rec := httptest.NewRecorder()
	s.queryTextHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("final callback: %d %s", rec.Code, rec.Body)
	}
}

const (
	doneVerdict = `{"schema":"decurix.verdict/v1","verdict":"done","by":"verify","why":"read back","basis":["code"],` +
		`"checks":[{"id":"files.read_back","result":"pass","basis":"code","detail":"out.csv: 3 rows read","refs":[]}],` +
		`"evidence":["Checked against the files I wrote: 3 rows"]}`
	stoppedVerdict = `{"schema":"decurix.verdict/v1","verdict":"stopped","by":"worker","why":"the person said no",` +
		`"basis":[],"checks":[{"id":"approval","result":"pass","basis":"person","detail":"asked","refs":[]}],"evidence":[]}`
)

// A stop at a person's no is never the work done in a handoff or the work queue, whatever its report or its check
// said; a done verdict's evidence is what the handoff hands back (R6: no report text is read for either).
func TestTheVerdictDecidesHandoffAndQueueOutcomes(t *testing.T) {
	s := chatServer(t)
	w := tasks.Worker{ID: "box", Name: "box", Engine: "thebot"}
	open := func() string {
		route, err := s.Tasks.Commit(tasks.NewRoute(), w, "summarise the sheet", tasks.Origin{Kind: tasks.OriginPerson})
		if err != nil {
			t.Fatal(err)
		}
		return route.TaskID
	}
	ha, qa := handoffAdapter{s}, queueAdapter{s}

	done := open()
	finalWithVerdict(t, s, done, "Wrote out.csv.", doneVerdict, true)
	o, err := ha.TaskOutcome(done)
	if err != nil || o.Checked == nil || !*o.Checked {
		t.Fatalf("a checked done = %+v, %v", o, err)
	}
	if len(o.Evidence) != 2 || o.Evidence[0] != "Checked against the files I wrote: 3 rows" || o.Evidence[1] != "out.csv: 3 rows read" {
		t.Fatalf("evidence = %q", o.Evidence)
	}
	if st, _ := qa.TaskState(done); st.Checked == nil || !*st.Checked {
		t.Fatalf("queue state of a checked done = %+v", st)
	}

	// The report reads like work done, and the desktop even said checked: the verdict says it stopped.
	stopped := open()
	finalWithVerdict(t, s, stopped, "Paid the invoice.", stoppedVerdict, true)
	if o, _ := ha.TaskOutcome(stopped); o.Checked == nil || *o.Checked {
		t.Fatalf("a stop = %+v", o)
	}
	if st, _ := qa.TaskState(stopped); st.Checked == nil || *st.Checked {
		t.Fatalf("queue state of a stop = %+v", st)
	}

	d, _ := s.Tasks.Get(stopped)
	if d.Executions[0].WorkDone() || d.Executions[0].Verdict.Verdict != tasks.GateStopped {
		t.Fatalf("stored = %+v", d.Executions[0])
	}

	// An older image's checked-only final (no verdict) fails closed: stored not checked, since nothing shows what was
	// checked.
	old := open()
	finalWithVerdict(t, s, old, "Done.", `null`, true)
	if o, _ := ha.TaskOutcome(old); o.Checked == nil || *o.Checked || o.Evidence != nil {
		t.Fatalf("an old image's checked done = %+v", o)
	}
	if st, _ := qa.TaskState(old); st.Checked != nil && *st.Checked {
		t.Fatalf("queue state of an old image's checked done = %+v", st)
	}
}

// A readiness run is stored by its grade: only a passed run is done and checked; a failed one is not done, never
// checked (it was, whenever the run finished), and one that could not be finished is unchecked.
func TestAReadinessRunIsStoredByItsGrade(t *testing.T) {
	for _, c := range []struct {
		err    error
		status string
		want   string
	}{{nil, "passed", tasks.GateDone}, {nil, "failed", tasks.GateNotDone}, {errors.New("disk full"), "", tasks.GateUnchecked}} {
		s := chatServer(t)
		route, _ := s.Tasks.Commit(tasks.NewRoute(), tasks.Worker{ID: "readiness-r1", Name: "r"}, "Readiness run", tasks.Origin{Kind: tasks.OriginPerson})
		v := readinessVerdict(c.err, c.status, "r1")
		_, _ = s.Tasks.RecordOutcome("readiness-r1", route.TaskID, tasks.Outcome{Kind: tasks.OutcomeDone, Reply: "x", Verdict: v})
		d, _ := s.Tasks.Get(route.TaskID)
		e := d.Executions[0]
		if e.Verdict == nil || e.Verdict.Verdict != c.want || e.Verdict.By != tasks.ByReadiness || (*e.Checked) != (c.want == tasks.GateDone) {
			t.Errorf("%v %q: stored %+v checked %v", c.err, c.status, e.Verdict, *e.Checked)
		}
	}
}
