package qualification

import (
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

func testDefinition() Definition {
	cases := []Case{}
	for _, category := range []string{"ordinary", "varied", "missing_input", "invalid_input", "injection"} {
		c := Case{ID: strings.ReplaceAll(category, "_", "-"), Category: category, Expected: "complete", Inputs: map[string][]byte{}, Checks: []Check{{File: "result.txt", Kind: "text_equals", Text: "correct"}}}
		if category == "missing_input" || category == "invalid_input" {
			c.Expected = "escalate"
			c.Checks = nil
		}
		cases = append(cases, c)
	}
	return Definition{Name: "Reconcile", WorkerID: "worker", SourceName: "reconcile", Environment: "Company local drafts", Scope: "Reconcile supplied files", Language: "python", Script: "print('task')", Image: "sha256:" + strings.Repeat("a", 64), Runtime: RuntimeVersion, TimeoutSeconds: 5, Cases: cases}
}

func testStore(t *testing.T) *Store {
	t.Helper()
	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	return storeOn(t, db)
}

// fileStore is a Store on a database file opened as the control plane opens
// it (sqlitedb.Open: a pool of connections, WAL, BEGIN IMMEDIATE), for a test
// of two callers at once: OpenMemory's pool is one connection, which
// serialises every caller, so a race there cannot happen and its test cannot
// fail (review 2026-11-02).
func fileStore(t *testing.T) *Store {
	t.Helper()
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "qualification.db"))
	if err != nil {
		t.Fatal(err)
	}
	return storeOn(t, db)
}

func storeOn(t *testing.T, db *sql.DB) *Store {
	t.Helper()
	t.Cleanup(func() { db.Close() })
	s, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func successfulFindings(d Definition) []Finding {
	out := []Finding{}
	for _, c := range d.Cases {
		result := Result{ExitCode: 0, Artifacts: map[string][]byte{"result.txt": []byte("correct")}}
		if c.Expected == "escalate" {
			result.ExitCode = 20
			result.Artifacts = nil
		}
		out = append(out, Verify(c, result, nil))
	}
	return out
}

func qualify(t *testing.T, s *Store) Responsibility {
	t.Helper()
	r, err := s.Save("", testDefinition())
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.BeginRun(r.ID, r.Revision, "practice", "practice-task")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.FinishRun(run.ID, successfulFindings(r.Definition), nil); err != nil {
		t.Fatal(err)
	}
	r, err = s.Get(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestReadinessNeverGrantsPermission(t *testing.T) {
	s := testStore(t)
	r := qualify(t, s)
	if r.Readiness != Qualified || r.Authorization != Supervised {
		t.Fatalf("unexpected states: %+v", r)
	}
	if _, err := s.BeginRun(r.ID, r.Revision, "production", "task"); err != ErrDenied {
		t.Fatalf("ungated production: %v", err)
	}
	if _, err := s.Authorize(r.ID, "old-revision", "enable"); err != ErrDenied {
		t.Fatal("stale revision enabled")
	}
	if _, err := s.Authorize(r.ID, r.Revision, "enable"); err != nil {
		t.Fatal(err)
	}
	run, err := s.BeginRun(r.ID, r.Revision, "production", "task")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.FinishRun(run.ID, []Finding{{CaseID: "prod", Passed: false}}, nil); err != nil {
		t.Fatal(err)
	}
	r, _ = s.Get(r.ID)
	if r.Authorization != Suspended || r.Readiness != NeedsRetesting {
		t.Fatal("failed production retained permission")
	}
}

func TestSuiteCompletenessAndRevisionInvalidation(t *testing.T) {
	s := testStore(t)
	r, err := s.Save("", testDefinition())
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.BeginRun(r.ID, r.Revision, "practice", "task")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.FinishRun(run.ID, successfulFindings(r.Definition)[:4], nil); err != nil {
		t.Fatal(err)
	}
	r, _ = s.Get(r.ID)
	if r.Readiness == Qualified {
		t.Fatal("partial suite qualified")
	}
	r = qualify(t, s)
	_, _ = s.Authorize(r.ID, r.Revision, "enable")
	d := r.Definition
	d.Script += "\n# changed behavior"
	changed, err := s.Save(r.ID, d)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Readiness != NeedsRetesting || changed.Authorization != Supervised || changed.Revision == r.Revision {
		t.Fatal("revision retained old grant")
	}
}

func TestConcurrentRunsAndDuplicateCompletion(t *testing.T) {
	s := fileStore(t)
	r := qualify(t, s)
	_, _ = s.Authorize(r.ID, r.Revision, "enable")
	const callers = 8
	var wg sync.WaitGroup
	results := make(chan error, callers)
	runs := make(chan Run, callers)
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			run, err := s.BeginRun(r.ID, r.Revision, "production", "task")
			results <- err
			if err == nil {
				runs <- run
			}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(runs)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if err != ErrConflict {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("%d concurrent claims", successes)
	}
	run := <-runs
	if _, err := s.FinishRun(run.ID, []Finding{{Passed: true}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishRun(run.ID, []Finding{{Passed: true}}, nil); err != ErrConflict {
		t.Fatal("duplicate completion accepted")
	}
}

func TestWrongVerdictDuringExamCannotBeOverwritten(t *testing.T) {
	s := testStore(t)
	r := qualify(t, s)
	run, err := s.BeginRun(r.ID, r.Revision, "practice", "task")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.WrongTask("task"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.FinishRun(run.ID, successfulFindings(r.Definition), nil); err != nil {
		t.Fatal(err)
	}
	r, _ = s.Get(r.ID)
	if r.Authorization != Suspended || r.Readiness != NeedsRetesting {
		t.Fatal("exam overrode wrong-result suspension")
	}
}

func TestRestartAndExpiryFailClosed(t *testing.T) {
	s := testStore(t)
	r := qualify(t, s)
	_, _ = s.Authorize(r.ID, r.Revision, "enable")
	restarted, err := NewStore(s.db)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := restarted.Get(r.ID)
	if after.Authorization != Supervised || after.Readiness != Qualified {
		t.Fatal("restart did not require owner review")
	}
	if _, err = restarted.Authorize(r.ID, r.Revision, "enable"); err != nil {
		t.Fatal(err)
	}
	run, err := restarted.BeginRun(r.ID, r.Revision, "production", "task")
	if err != nil {
		t.Fatal(err)
	}
	restarted, err = NewStore(s.db)
	if err != nil {
		t.Fatal(err)
	}
	after, _ = restarted.Get(r.ID)
	e, _ := restarted.Run(run.ID)
	if after.Authorization != Suspended || e.Status != "interrupted" {
		t.Fatal("interrupted run retained authority")
	}
	r = qualify(t, restarted)
	restarted.now = func() time.Time { return r.QualifiedAt.Add(31 * 24 * time.Hour) }
	if _, err = restarted.Authorize(r.ID, r.Revision, "enable"); err != ErrDenied {
		t.Fatal("expired evidence enabled")
	}
}

func TestFingerprintAndSuiteValidation(t *testing.T) {
	d := testDefinition()
	if err := Validate(d); err != nil {
		t.Fatal(err)
	}
	original := Fingerprint(d)
	d.Cases[0].Checks[0].Text = "different"
	if Fingerprint(d) == original {
		t.Fatal("expected results not pinned")
	}
	d.Cases = d.Cases[:4]
	if Validate(d) == nil {
		t.Fatal("missing injection category accepted")
	}
	c := testDefinition().Cases[0]
	c.Inputs = map[string][]byte{"../secret": []byte("x")}
	if ValidateCase(c) == nil {
		t.Fatal("traversal accepted")
	}
	c = testDefinition().Cases[0]
	c.Inputs = map[string][]byte{"result.txt": {}}
	if ValidateCase(c) == nil {
		t.Fatal("output aliases input")
	}
}

func TestCancelledRunCannotRequalifyOrDoubleFinish(t *testing.T) {
	s := testStore(t)
	r := qualify(t, s)
	run, err := s.BeginRun(r.ID, r.Revision, "practice", "task")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Cancel(run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.FinishRun(run.ID, successfulFindings(r.Definition), nil); err != ErrConflict {
		t.Fatal("late success overwrote cancellation")
	}
	r, _ = s.Get(r.ID)
	if r.Readiness != NeedsRetesting || r.Authorization != Supervised {
		t.Fatal("cancelled run retained readiness")
	}
	history, err := s.History(r.ID)
	if err != nil || history[0].Status != "cancelled" {
		t.Fatal("cancelled evidence disappeared")
	}
}

func TestCaseEvidenceSurvivesInterruption(t *testing.T) {
	s := testStore(t)
	r, err := s.Save("", testDefinition())
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.BeginRun(r.ID, r.Revision, "practice", "task")
	if err != nil {
		t.Fatal(err)
	}
	f := successfulFindings(r.Definition)[0]
	if err = s.RecordCase(run.ID, f, map[string][]byte{"result.txt": []byte("correct")}); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordCase(run.ID, f, nil); err != ErrConflict {
		t.Fatal("duplicate case accepted")
	}
	restarted, err := NewStore(s.db)
	if err != nil {
		t.Fatal(err)
	}
	e, err := restarted.Run(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	saved, ok, err := restarted.CaseArtifact(run.ID, f.CaseID, "result.txt")
	if e.Status != "interrupted" || len(e.Findings) != 1 || err != nil || !ok || string(saved) != "correct" {
		t.Fatal("completed case evidence lost on restart")
	}
	// The case's bytes are in their own row, not re-marshalled with the run on every case (C-18).
	if len(e.CaseArtifacts) != 0 {
		t.Fatalf("case files kept in the run's JSON: %v", e.CaseArtifacts)
	}
	if names, err := restarted.CaseFiles(run.ID); err != nil || len(names[f.CaseID]) != 1 || names[f.CaseID][0] != "result.txt" {
		t.Fatalf("CaseFiles = %v, %v", names, err)
	}
}
