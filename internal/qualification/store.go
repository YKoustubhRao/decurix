package qualification

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

var schema = []string{
	`CREATE TABLE qualification_responsibilities (id TEXT PRIMARY KEY, worker_id TEXT NOT NULL, data TEXT NOT NULL);
	 CREATE TABLE qualification_runs (id TEXT PRIMARY KEY, responsibility_id TEXT NOT NULL REFERENCES qualification_responsibilities(id), task_id TEXT NOT NULL, status TEXT NOT NULL, data TEXT NOT NULL);
	 CREATE INDEX qualification_runs_task ON qualification_runs(task_id);
	 CREATE TABLE qualification_events (id INTEGER PRIMARY KEY, responsibility_id TEXT NOT NULL, at_ns INTEGER NOT NULL, actor TEXT NOT NULL, action TEXT NOT NULL, reason TEXT NOT NULL);`,
	// Step 2: practice from a worker's taught skills and recordings (simstore.go).
	`CREATE TABLE simulation_responsibilities (id TEXT PRIMARY KEY, worker_id TEXT NOT NULL, source_kind TEXT NOT NULL, source_name TEXT NOT NULL, data TEXT NOT NULL, UNIQUE(worker_id, source_kind, source_name));
	 CREATE TABLE simulation_suites (id TEXT PRIMARY KEY, responsibility_id TEXT NOT NULL, status TEXT NOT NULL, data TEXT NOT NULL);
	 CREATE INDEX simulation_suites_responsibility ON simulation_suites(responsibility_id);
	 CREATE TABLE simulation_runs (id TEXT PRIMARY KEY, responsibility_id TEXT NOT NULL, suite_id TEXT NOT NULL, status TEXT NOT NULL, data TEXT NOT NULL);
	 CREATE INDEX simulation_runs_responsibility ON simulation_runs(responsibility_id);
	 CREATE TABLE simulation_findings (run_id TEXT NOT NULL, case_id TEXT NOT NULL, position INTEGER NOT NULL, task_id TEXT NOT NULL, data TEXT NOT NULL, PRIMARY KEY (run_id, case_id));
	 CREATE INDEX simulation_findings_task ON simulation_findings(task_id);
	 CREATE TABLE simulation_files (run_id TEXT NOT NULL, case_id TEXT NOT NULL, role TEXT NOT NULL, name TEXT NOT NULL, sha256 TEXT NOT NULL, body BLOB NOT NULL, PRIMARY KEY (run_id, case_id, role, name));
	 CREATE TABLE simulation_holds (id TEXT PRIMARY KEY, worker_id TEXT NOT NULL, request_key TEXT NOT NULL, status TEXT NOT NULL, data TEXT NOT NULL, UNIQUE(worker_id, request_key));`,
}

// Store owns durable readiness records and serializes state transitions.
type Store struct {
	db  *sql.DB
	mu  sync.Mutex
	now func() time.Time
}

// NewStore migrates the store and fails closed after interrupted work or restore.
// Evidence survives; enabling after a control-plane restart requires owner review.
func NewStore(db *sql.DB) (*Store, error) {
	if err := sqlitedb.Migrate(db, "qualification", schema); err != nil {
		return nil, err
	}
	s := &Store{db: db, now: func() time.Time { return time.Now().UTC() }}
	list, err := s.List("")
	if err != nil {
		return nil, err
	}
	for _, r := range list {
		if r.Readiness == Testing || r.Authorization == Enabled {
			if r.Readiness == Testing {
				r.Readiness = NeedsRetesting
			}
			r.Authorization, r.Reason = Supervised, "Control plane restarted; review evidence before enabling again."
			if err := s.save(r, "control_plane", "restart", r.Reason); err != nil {
				return nil, err
			}
		}
	}
	rows, err := db.Query(`SELECT data FROM qualification_runs WHERE status = 'running'`)
	if err != nil {
		return nil, err
	}
	var interrupted []Run
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			rows.Close()
			return nil, err
		}
		var run Run
		if err := json.Unmarshal([]byte(b), &run); err != nil {
			rows.Close()
			return nil, err
		}
		interrupted = append(interrupted, run)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, run := range interrupted {
		run.Status, run.EndedAt = "interrupted", s.now()
		if err := s.saveRun(run); err != nil {
			return nil, err
		}
		if run.Mode == "production" {
			if err := s.Invalidate(run.ResponsibilityID, "An enabled run was interrupted; reconcile its result and retest.", true); err != nil {
				return nil, err
			}
		}
	}
	if err := s.recoverPractice(); err != nil {
		return nil, err
	}
	return s, nil
}

func newID(prefix string) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b)
}

// List returns a worker's responsibilities, including expired readiness.
func (s *Store) List(worker string) ([]Responsibility, error) {
	rows, err := s.db.Query(`SELECT data FROM qualification_responsibilities WHERE (? = '' OR worker_id = ?) ORDER BY id`, worker, worker)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Responsibility{}
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		var r Responsibility
		if err := json.Unmarshal([]byte(b), &r); err != nil {
			return nil, err
		}
		s.expire(&r)
		out = append(out, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	active, err := s.db.Query(`SELECT responsibility_id,id FROM qualification_runs WHERE status='running'`)
	if err != nil {
		return nil, err
	}
	defer active.Close()
	byID := map[string]string{}
	for active.Next() {
		var responsibility, id string
		if err := active.Scan(&responsibility, &id); err != nil {
			return nil, err
		}
		byID[responsibility] = id
	}
	if err := active.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].ActiveRunID = byID[out[i].ID]
	}
	return out, nil
}

func (s *Store) expire(r *Responsibility) {
	if r.Readiness == Qualified && (r.Definition.Runtime != RuntimeVersion || s.now().Sub(r.QualifiedAt) > 30*24*time.Hour) {
		r.Readiness, r.Authorization, r.Reason = NeedsRetesting, Supervised, "Qualification expired or the execution policy changed; retest."
	}
}

// Get returns a responsibility; stale evidence never yields an enabled grant.
func (s *Store) Get(id string) (Responsibility, error) {
	var b string
	err := s.db.QueryRow(`SELECT data FROM qualification_responsibilities WHERE id = ?`, id).Scan(&b)
	if err == sql.ErrNoRows {
		return Responsibility{}, ErrNotFound
	}
	if err != nil {
		return Responsibility{}, err
	}
	var r Responsibility
	if err := json.Unmarshal([]byte(b), &r); err != nil {
		return r, err
	}
	s.expire(&r)
	return r, nil
}

func (s *Store) save(r Responsibility, actor, action, reason string) error {
	r.ActiveRunID = ""
	r.UpdatedAt = s.now()
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO qualification_responsibilities(id,worker_id,data) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET worker_id=excluded.worker_id,data=excluded.data`, r.ID, r.Definition.WorkerID, string(b)); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO qualification_events(responsibility_id,at_ns,actor,action,reason) VALUES(?,?,?,?,?)`, r.ID, sqlitedb.ToNS(r.UpdatedAt), actor, action, reason); err != nil {
		return err
	}
	return tx.Commit()
}

// Save creates or revises an owner-reviewed definition, revoking old authority.
func (s *Store) Save(id string, d Definition) (Responsibility, error) {
	if err := Validate(d); err != nil {
		return Responsibility{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := Responsibility{ID: id, Definition: d, Revision: Fingerprint(d), Readiness: Untested, Authorization: Supervised, UpdatedAt: s.now()}
	if id == "" {
		r.ID = newID("r")
	} else {
		old, err := s.Get(id)
		if err != nil {
			return r, err
		}
		busy, err := s.busy(id)
		if err != nil {
			return r, err
		}
		if busy {
			return r, ErrConflict
		}
		if old.Revision == r.Revision {
			return old, nil
		}
		r.Readiness, r.Reason = NeedsRetesting, "Executable, environment, scope or test suite changed."
	}
	return r, s.save(r, "owner", "define", r.Reason)
}

func (s *Store) busy(id string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM qualification_runs WHERE responsibility_id=? AND status='running'`, id).Scan(&n)
	return n > 0, err
}

// Authorize enables only current, complete evidence, or suspends a responsibility.
func (s *Store) Authorize(id, revision, action string) (Responsibility, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.Get(id)
	if err != nil {
		return r, err
	}
	busy, err := s.busy(id)
	if err != nil {
		return r, err
	}
	if busy {
		return r, ErrConflict
	}
	if action == "enable" {
		if r.Revision != revision || r.Readiness != Qualified || r.EvaluationID == "" {
			return r, ErrDenied
		}
		evidence, err := s.Run(r.EvaluationID)
		if err != nil {
			return r, err
		}
		if evidence.Mode != "practice" || evidence.Status != "passed" || evidence.Revision != r.Revision {
			return r, ErrDenied
		}
		r.Authorization, r.Reason = Enabled, "Owner enabled this revision for offline local artifacts only."
	} else if action == "suspend" {
		r.Authorization, r.Reason = Suspended, "Owner suspended this responsibility."
	} else {
		return r, ErrInvalid
	}
	return r, s.save(r, "owner", action, r.Reason)
}

// Invalidate revokes permission when behavior or observed correctness changes.
func (s *Store) Invalidate(id, reason string, suspend bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.Get(id)
	if err != nil {
		return err
	}
	r.Readiness, r.Authorization, r.Reason = NeedsRetesting, Supervised, reason
	if suspend {
		r.Authorization = Suspended
	}
	return s.save(r, "control_plane", "invalidate", reason)
}

// BeginRun atomically claims the responsibility and freezes its revision.
func (s *Store) BeginRun(id, revision, mode, taskID string) (Run, error) {
	return s.BeginExecution(id, revision, mode, taskID, nil)
}

// BeginExecution claims a run with an optional frozen production case.
func (s *Store) BeginExecution(id, revision, mode, taskID string, production *Case) (Run, error) {
	if production != nil && (mode != "production" || production.Category != "production" || ValidateCase(*production) != nil) {
		return Run{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.Get(id)
	if err != nil {
		return Run{}, err
	}
	busy, err := s.busy(id)
	if err != nil {
		return Run{}, err
	}
	if busy {
		return Run{}, ErrConflict
	}
	if revision != r.Revision {
		return Run{}, ErrDenied
	}
	if mode == "production" {
		if r.Readiness != Qualified || r.Authorization != Enabled {
			return Run{}, ErrDenied
		}
	} else if mode != "practice" {
		return Run{}, ErrInvalid
	}
	run := Run{ID: newID("e"), ResponsibilityID: id, TaskID: taskID, Mode: mode, Revision: r.Revision, Definition: r.Definition, Status: "running", Findings: []Finding{}, StartedAt: s.now()}
	run.ProductionCase = production
	b, err := json.Marshal(run)
	if err != nil {
		return run, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return run, err
	}
	defer tx.Rollback()
	if mode == "practice" {
		r.Readiness, r.Authorization, r.Reason, r.EvaluationID = Testing, Supervised, "Independent evaluation is running.", run.ID
	}
	r.UpdatedAt = s.now()
	rb, err := json.Marshal(r)
	if err != nil {
		return run, err
	}
	if _, err := tx.Exec(`UPDATE qualification_responsibilities SET data=? WHERE id=?`, string(rb), id); err != nil {
		return run, err
	}
	if _, err := tx.Exec(`INSERT INTO qualification_runs(id,responsibility_id,task_id,status,data) VALUES(?,?,?,?,?)`, run.ID, id, taskID, run.Status, string(b)); err != nil {
		return run, err
	}
	return run, tx.Commit()
}

func (s *Store) saveRun(run Run) error {
	b, err := json.Marshal(run)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE qualification_runs SET status=?,data=? WHERE id=?`, run.Status, string(b), run.ID)
	return err
}

// RecordCase persists each independent finding before moving to the next case.
// It preserves reviewed bytes across a restart and bounds evidence per run.
func (s *Store) RecordCase(id string, finding Finding, artifacts map[string][]byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, err := s.Run(id)
	if err != nil {
		return err
	}
	if run.Status != "running" {
		return ErrConflict
	}
	index := len(run.Findings)
	if run.Mode == "practice" && (index >= len(run.Definition.Cases) || run.Definition.Cases[index].ID != finding.CaseID) {
		return ErrConflict
	}
	if run.Mode == "production" && index != 0 {
		return ErrConflict
	}
	total := 0
	for _, files := range run.CaseArtifacts {
		for _, b := range files {
			total += len(b)
		}
	}
	for n, b := range artifacts {
		if !FileName(n) || len(b) > MaxFileBytes {
			return ErrInvalid
		}
		total += len(b)
	}
	if total > MaxEvidenceBytes {
		return ErrInvalid
	}
	if run.CaseArtifacts == nil {
		run.CaseArtifacts = map[string]map[string][]byte{}
	}
	run.CaseArtifacts[finding.CaseID] = artifacts
	run.Findings = append(run.Findings, finding)
	return s.saveRun(run)
}

// FinishRun accepts only findings for every frozen case and never auto-enables.
func (s *Store) FinishRun(id string, findings []Finding, artifacts map[string][]byte) (Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, err := s.Run(id)
	if err != nil {
		return run, err
	}
	if run.Status != "running" {
		return run, ErrConflict
	}
	r, err := s.Get(run.ResponsibilityID)
	if err != nil {
		return run, err
	}
	passed := len(findings) > 0
	if run.Mode == "practice" {
		passed = len(findings) == len(run.Definition.Cases)
		for i, f := range findings {
			if i >= len(run.Definition.Cases) || f.CaseID != run.Definition.Cases[i].ID || !f.Passed {
				passed = false
			}
		}
	} else {
		passed = len(findings) == 1 && findings[0].Passed
	}
	run.Findings, run.Artifacts, run.EndedAt, run.Status = findings, artifacts, s.now(), "failed"
	if passed {
		run.Status = "passed"
	}
	if run.Mode == "practice" && r.EvaluationID == run.ID && r.Revision == run.Revision && r.Readiness == Testing {
		r.Readiness, r.Authorization, r.Reason = NeedsRetesting, Supervised, "One or more cases failed; review the evidence and retest."
		if passed {
			r.Readiness, r.QualifiedAt, r.Reason = Qualified, s.now(), "All reviewed cases passed independent artifact checks. Owner authorization is still required."
		}
	} else if run.Mode == "production" && !passed {
		r.Readiness, r.Authorization, r.Reason = NeedsRetesting, Suspended, "An enabled run failed its independent check; reconcile and retest."
	}
	r.UpdatedAt = s.now()
	// The run and grant transition must commit together, including on disk-full.
	tx, err := s.db.Begin()
	if err != nil {
		return run, err
	}
	defer tx.Rollback()
	b, err := json.Marshal(run)
	if err != nil {
		return run, err
	}
	rb, err := json.Marshal(r)
	if err != nil {
		return run, err
	}
	if _, err := tx.Exec(`UPDATE qualification_runs SET status=?,data=? WHERE id=?`, run.Status, string(b), run.ID); err != nil {
		return run, err
	}
	if _, err := tx.Exec(`UPDATE qualification_responsibilities SET data=? WHERE id=?`, string(rb), r.ID); err != nil {
		return run, err
	}
	if _, err := tx.Exec(`INSERT INTO qualification_events(responsibility_id,at_ns,actor,action,reason) VALUES(?,?,?,?,?)`, r.ID, sqlitedb.ToNS(s.now()), "verifier", run.Status, r.Reason); err != nil {
		return run, err
	}
	return run, tx.Commit()
}

// Cancel records a durable terminal state before stopping the container.
// A late successful callback cannot overwrite cancellation or re-enable a grant.
func (s *Store) Cancel(id string) (Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, err := s.Run(id)
	if err != nil {
		return run, err
	}
	if run.Status != "running" {
		return run, ErrConflict
	}
	r, err := s.Get(run.ResponsibilityID)
	if err != nil {
		return run, err
	}
	run.Status, run.EndedAt = "cancelled", s.now()
	r.Readiness, r.Authorization, r.Reason = NeedsRetesting, Supervised, "Owner cancelled the run; review its evidence and retest."
	r.UpdatedAt = s.now()
	if run.Mode == "production" {
		r.Authorization = Suspended
	}
	tx, err := s.db.Begin()
	if err != nil {
		return run, err
	}
	defer tx.Rollback()
	b, err := json.Marshal(run)
	if err != nil {
		return run, err
	}
	rb, err := json.Marshal(r)
	if err != nil {
		return run, err
	}
	if _, err = tx.Exec(`UPDATE qualification_runs SET status=?,data=? WHERE id=?`, run.Status, string(b), run.ID); err != nil {
		return run, err
	}
	if _, err = tx.Exec(`UPDATE qualification_responsibilities SET data=? WHERE id=?`, string(rb), r.ID); err != nil {
		return run, err
	}
	if _, err = tx.Exec(`INSERT INTO qualification_events(responsibility_id,at_ns,actor,action,reason) VALUES(?,?,?,?,?)`, r.ID, sqlitedb.ToNS(s.now()), "owner", "cancel", r.Reason); err != nil {
		return run, err
	}
	return run, tx.Commit()
}

// Run returns a frozen suite and independently collected evidence.
func (s *Store) Run(id string) (Run, error) {
	var b string
	err := s.db.QueryRow(`SELECT data FROM qualification_runs WHERE id=?`, id).Scan(&b)
	if err == sql.ErrNoRows {
		return Run{}, ErrNotFound
	}
	var run Run
	if err != nil {
		return run, err
	}
	err = json.Unmarshal([]byte(b), &run)
	return run, err
}

// ForTask returns the authority and evidence linked to an ordinary task record.
func (s *Store) ForTask(taskID string) (Run, error) {
	var id string
	err := s.db.QueryRow(`SELECT id FROM qualification_runs WHERE task_id=? ORDER BY rowid DESC LIMIT 1`, taskID).Scan(&id)
	if err == sql.ErrNoRows {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, err
	}
	return s.Run(id)
}

// WrongTask revokes a linked grant; retracting a verdict cannot restore it.
func (s *Store) WrongTask(taskID string) error {
	run, err := s.ForTask(taskID)
	if err == ErrNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	return s.Invalidate(run.ResponsibilityID, "A person marked a linked task wrong; review and retest.", true)
}

// History returns frozen evidence for a responsibility, newest first.
func (s *Store) History(id string) ([]Run, error) {
	rows, err := s.db.Query(`SELECT json_remove(data, '$.definition', '$.production_case', '$.artifacts', '$.case_artifacts') FROM qualification_runs WHERE (?='' OR responsibility_id=?) ORDER BY rowid DESC LIMIT 100`, id, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Run{}
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		var run Run
		if err := json.Unmarshal([]byte(b), &run); err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

// Interrupted returns every interrupted identity for task-record reconciliation.
func (s *Store) Interrupted() ([]Run, error) {
	rows, err := s.db.Query(`SELECT data FROM qualification_runs WHERE status='interrupted'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Run{}
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		var run Run
		if err := json.Unmarshal([]byte(b), &run); err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}
