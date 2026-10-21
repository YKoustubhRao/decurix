package qualification

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// PracticeFile is one piece of a case's evidence: an input, an output, the
// mock app's stored state or a practice draft. Body is served only on request.
type PracticeFile struct {
	CaseID string `json:"case_id"`
	Role   string `json:"role"`
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int    `json:"size"`
	Body   []byte `json:"-"`
}

// Practice file roles.
const (
	RoleInput     = "input"
	RoleOutput    = "output"
	RoleMockState = "mock_state"
	RoleDraft     = "draft"
)

// practiceExpiry is how long a clean practice run qualifies a revision (as readiness).
const practiceExpiry = 30 * 24 * time.Hour

// recoverPractice fails closed after a restart: a running practice run is
// interrupted, its responsibility needs retesting, and every enabled grant
// returns to supervision for the owner to review (readiness's rule).
func (s *Store) recoverPractice() error {
	runs, err := queryJSON[PracticeRun](s.db, `SELECT data FROM simulation_runs WHERE status='running'`)
	if err != nil {
		return err
	}
	for _, run := range runs {
		run.Status, run.EndedAt, run.Note = "interrupted", s.now(), "The control plane restarted during this run; it never counts."
		if err := s.savePracticeRun(run); err != nil {
			return err
		}
	}
	list, err := queryJSON[PracticeResponsibility](s.db, `SELECT data FROM simulation_responsibilities ORDER BY id`)
	if err != nil {
		return err
	}
	for _, r := range list {
		if r.Readiness != Testing && r.Authorization != Enabled {
			continue
		}
		if r.Readiness == Testing {
			r.Readiness = NeedsRetesting
		}
		r.Authorization, r.Reason = Supervised, "Control plane restarted; review evidence before enabling again."
		if err := s.savePractice(r, "control_plane", "restart", r.Reason); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) expirePractice(r *PracticeResponsibility) {
	if r.Readiness == Qualified && s.now().Sub(r.QualifiedAt) > practiceExpiry {
		r.Readiness, r.Authorization, r.Reason = NeedsRetesting, Supervised, "Practice evidence is older than 30 days; practise again."
	}
}

// ListPractice returns a worker's practice responsibilities ("" for all).
func (s *Store) ListPractice(worker string) ([]PracticeResponsibility, error) {
	list, err := queryJSON[PracticeResponsibility](s.db, `SELECT data FROM simulation_responsibilities WHERE (?='' OR worker_id=?) ORDER BY id`, worker, worker)
	if err != nil {
		return nil, err
	}
	active, err := queryJSON[PracticeRun](s.db, `SELECT data FROM simulation_runs WHERE status='running'`)
	if err != nil {
		return nil, err
	}
	for i := range list {
		s.expirePractice(&list[i])
		for _, run := range active {
			if run.ResponsibilityID == list[i].ID {
				list[i].ActiveRunID = run.ID
			}
		}
	}
	return list, nil
}

// GetPractice returns one practice responsibility; stale evidence grants nothing.
func (s *Store) GetPractice(id string) (PracticeResponsibility, error) {
	r, err := getJSON[PracticeResponsibility](s.db, `SELECT data FROM simulation_responsibilities WHERE id=?`, id)
	if err != nil {
		return r, err
	}
	s.expirePractice(&r)
	return r, nil
}

func (s *Store) practiceFor(worker, kind, name string) (PracticeResponsibility, error) {
	r, err := scanJSON[PracticeResponsibility](s.db.QueryRow(`SELECT data FROM simulation_responsibilities WHERE worker_id=? AND source_kind=? AND source_name=?`, worker, kind, name))
	s.expirePractice(&r)
	return r, err
}

func (s *Store) savePractice(r PracticeResponsibility, actor, action, reason string) error {
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
	if _, err := tx.Exec(`INSERT INTO simulation_responsibilities(id,worker_id,source_kind,source_name,data) VALUES(?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data`, r.ID, r.WorkerID, r.SourceKind, r.SourceName, string(b)); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO qualification_events(responsibility_id,at_ns,actor,action,reason) VALUES(?,?,?,?,?)`, r.ID, sqlitedb.ToNS(r.UpdatedAt), actor, action, reason); err != nil {
		return err
	}
	return tx.Commit()
}

// SaveDraft stores a generated suite for owner review. The store computes every
// hash itself, so a caller cannot pin bytes it did not hand over.
func (s *Store) SaveDraft(su PracticeSuite) (PracticeSuite, error) {
	if !namePattern.MatchString(su.WorkerID) || (su.Source.Kind != SourceSkill && su.Source.Kind != SourceRecording) || su.Source.Name == "" || su.Generator == "" {
		return su, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.practiceFor(su.WorkerID, su.Source.Kind, su.Source.Name)
	if errors.Is(err, sql.ErrNoRows) {
		r = PracticeResponsibility{ID: sqlitedb.NewID("p", 16), WorkerID: su.WorkerID, SourceKind: su.Source.Kind, SourceName: su.Source.Name,
			Title: su.Source.Title, Readiness: Untested, Authorization: Supervised, Reason: "No practice suite approved yet."}
		if err := s.savePractice(r, "owner", "create", r.Reason); err != nil {
			return su, err
		}
	} else if err != nil {
		return su, err
	}
	su.ID, su.ResponsibilityID, su.Status, su.CreatedAt = sqlitedb.NewID("s", 16), r.ID, SuiteDraft, s.now()
	su.Source.SHA256 = sourceFingerprint(su.Source)
	su.GeneratorSHA = SHA256Hex([]byte(su.Generator))
	su.MockSHA = ""
	if su.Mock != "" {
		su.MockSHA = SHA256Hex([]byte(su.Mock))
	}
	su.CasesSHA = CasesFingerprint(su.Cases)
	su.Revision = SuiteRevision(su)
	if su.Problems == nil {
		su.Problems = []string{}
	}
	b, err := json.Marshal(su)
	if err != nil {
		return su, err
	}
	_, err = s.db.Exec(`INSERT INTO simulation_suites(id,responsibility_id,status,data) VALUES(?,?,?,?)`, su.ID, su.ResponsibilityID, su.Status, string(b))
	return su, err
}

// sourceFingerprint hashes the source's exported bytes and how it was described.
func sourceFingerprint(src PracticeSource) string {
	b, _ := json.Marshal(struct {
		Kind  string            `json:"kind"`
		Name  string            `json:"name"`
		Files map[string][]byte `json:"files"`
		Brief string            `json:"brief"`
	}{src.Kind, src.Name, src.Files, src.Brief})
	return SHA256Hex(b)
}

// Suite returns one stored suite.
func (s *Store) Suite(id string) (PracticeSuite, error) {
	return getJSON[PracticeSuite](s.db, `SELECT data FROM simulation_suites WHERE id=?`, id)
}

func (s *Store) saveSuite(tx *sql.Tx, su PracticeSuite) error {
	b, err := json.Marshal(su)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE simulation_suites SET status=?,data=? WHERE id=?`, su.Status, string(b), su.ID)
	return err
}

// ApproveSuite pins a draft the owner reviewed. The hashes the owner saw must
// equal the stored ones; a draft with problems is never approvable.
func (s *Store) ApproveSuite(id, generatorSHA, mockSHA, casesSHA string) (PracticeResponsibility, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	su, err := s.Suite(id)
	if err != nil {
		return PracticeResponsibility{}, err
	}
	if su.Status != SuiteDraft || len(su.Problems) > 0 || su.GeneratorSHA != generatorSHA || su.MockSHA != mockSHA || su.CasesSHA != casesSHA {
		return PracticeResponsibility{}, ErrDenied
	}
	r, err := s.GetPractice(su.ResponsibilityID)
	if err != nil {
		return r, err
	}
	if err := s.idle("simulation_runs", r.ID); err != nil {
		return r, err
	}
	// Other approved suites of this responsibility are retired.
	older, err := queryJSON[PracticeSuite](s.db, `SELECT data FROM simulation_suites WHERE responsibility_id=? AND status=?`, r.ID, SuiteApproved)
	if err != nil {
		return r, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return r, err
	}
	defer tx.Rollback()
	for _, o := range older {
		o.Status, o.Reason = SuiteRetired, "Replaced by a newer approved suite."
		if err := s.saveSuite(tx, o); err != nil {
			return r, err
		}
	}
	su.Status, su.ApprovedAt = SuiteApproved, s.now()
	if err := s.saveSuite(tx, su); err != nil {
		return r, err
	}
	if err := tx.Commit(); err != nil {
		return r, err
	}
	readiness := Untested
	if r.Revision != "" {
		readiness = NeedsRetesting
	}
	r.Revision, r.SuiteID, r.SourceSHA, r.Title = su.Revision, su.ID, su.Source.SHA256, su.Source.Title
	r.Readiness, r.Authorization, r.EvaluationID = readiness, Supervised, ""
	r.Reason = "Suite approved by the owner; run practice to qualify this revision."
	return r, s.savePractice(r, "owner", "approve", r.Reason)
}

// Suites lists a responsibility's suites, newest first, without their case bytes.
func (s *Store) Suites(responsibilityID string) ([]PracticeSuite, error) {
	list, err := queryJSON[PracticeSuite](s.db, `SELECT data FROM simulation_suites WHERE responsibility_id=? ORDER BY rowid DESC LIMIT 50`, responsibilityID)
	for i := range list {
		for j := range list[i].Cases {
			list[i].Cases[j].Files = nil
		}
		list[i].Source.Files = nil
	}
	return list, err
}

// RetireSuite withdraws a suite (an owner found a wrong expectation). Findings
// stay as they were; the responsibility needs a new suite and a new run.
func (s *Store) RetireSuite(id, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	su, err := s.Suite(id)
	if err != nil {
		return err
	}
	r, err := s.GetPractice(su.ResponsibilityID)
	if err != nil {
		return err
	}
	if err := s.idle("simulation_runs", r.ID); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	su.Status, su.Reason = SuiteRetired, reason
	if err := s.saveSuite(tx, su); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if r.SuiteID != su.ID {
		return nil
	}
	r.SuiteID, r.Revision, r.Readiness, r.Authorization, r.EvaluationID = "", "", NeedsRetesting, Supervised, ""
	r.Reason = "The owner retired the suite: " + reason
	return s.savePractice(r, "owner", "retire", r.Reason)
}

// BeginPracticeRun claims the one practice slot of this control plane and
// freezes the responsibility's current revision.
func (s *Store) BeginPracticeRun(responsibilityID, revision string) (PracticeRun, PracticeSuite, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM simulation_runs WHERE status='running'`).Scan(&n); err != nil {
		return PracticeRun{}, PracticeSuite{}, err
	}
	if n > 0 {
		return PracticeRun{}, PracticeSuite{}, ErrConflict
	}
	r, err := s.GetPractice(responsibilityID)
	if err != nil {
		return PracticeRun{}, PracticeSuite{}, err
	}
	if r.Revision == "" || r.Revision != revision {
		return PracticeRun{}, PracticeSuite{}, ErrDenied
	}
	su, err := s.Suite(r.SuiteID)
	if err != nil {
		return PracticeRun{}, PracticeSuite{}, err
	}
	if su.Status != SuiteApproved || su.Revision != revision {
		return PracticeRun{}, PracticeSuite{}, ErrDenied
	}
	run := PracticeRun{ID: sqlitedb.NewID("x", 16), ResponsibilityID: r.ID, SuiteID: su.ID, Revision: revision, Status: "running", Of: len(su.Cases), Findings: []PracticeFinding{}, StartedAt: s.now()}
	b, err := json.Marshal(run)
	if err != nil {
		return run, su, err
	}
	r.Readiness, r.Authorization, r.EvaluationID, r.Reason = Testing, Supervised, run.ID, "Practice is running."
	r.UpdatedAt = s.now()
	rb, err := json.Marshal(r)
	if err != nil {
		return run, su, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return run, su, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO simulation_runs(id,responsibility_id,suite_id,status,data) VALUES(?,?,?,?,?)`, run.ID, r.ID, su.ID, run.Status, string(b)); err != nil {
		return run, su, err
	}
	if _, err := tx.Exec(`UPDATE simulation_responsibilities SET data=? WHERE id=?`, string(rb), r.ID); err != nil {
		return run, su, err
	}
	if _, err := tx.Exec(`INSERT INTO qualification_events(responsibility_id,at_ns,actor,action,reason) VALUES(?,?,?,?,?)`, r.ID, sqlitedb.ToNS(s.now()), "owner", "practice", r.Reason); err != nil {
		return run, su, err
	}
	return run, su, tx.Commit()
}

func (s *Store) savePracticeRun(run PracticeRun) error {
	run.Findings = nil
	b, err := json.Marshal(run)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE simulation_runs SET status=?,data=? WHERE id=?`, run.Status, string(b), run.ID)
	return err
}

func (s *Store) practiceRunRow(id string) (PracticeRun, error) {
	return getJSON[PracticeRun](s.db, `SELECT data FROM simulation_runs WHERE id=?`, id)
}

// PracticeProgress records which case is running, for the page's progress strip.
func (s *Store) PracticeProgress(runID string, n int, current string, tokens int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, err := s.practiceRunRow(runID)
	if err != nil {
		return err
	}
	if run.Status != "running" {
		return ErrConflict
	}
	run.Case, run.Current, run.Tokens = n, current, tokens
	return s.savePracticeRun(run)
}

// RecordPracticeCase persists one finding and its evidence before the next case.
func (s *Store) RecordPracticeCase(runID string, f PracticeFinding, files []PracticeFile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, err := s.practiceRunRow(runID)
	if err != nil {
		return err
	}
	if run.Status != "running" {
		return ErrConflict
	}
	su, err := s.Suite(run.SuiteID)
	if err != nil {
		return err
	}
	var position, stored int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM simulation_findings WHERE run_id=?`, runID).Scan(&position); err != nil {
		return err
	}
	if position >= len(su.Cases) || su.Cases[position].ID != f.CaseID {
		return ErrConflict
	}
	if err := s.db.QueryRow(`SELECT COALESCE(SUM(LENGTH(body)),0) FROM simulation_files WHERE run_id=?`, runID).Scan(&stored); err != nil {
		return err
	}
	for _, file := range files {
		stored += len(file.Body)
		if len(file.Body) > MaxFileBytes || file.Name == "" || len(file.Name) > 200 {
			return ErrInvalid
		}
	}
	if stored > MaxEvidenceBytes {
		return ErrInvalid
	}
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO simulation_findings(run_id,case_id,position,task_id,data) VALUES(?,?,?,?,?)`, runID, f.CaseID, position, f.TaskID, string(b)); err != nil {
		return err
	}
	for _, file := range files {
		if _, err := tx.Exec(`INSERT INTO simulation_files(run_id,case_id,role,name,sha256,body) VALUES(?,?,?,?,?,?)`, runID, f.CaseID, file.Role, file.Name, SHA256Hex(file.Body), file.Body); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) practiceFindings(runID string) ([]PracticeFinding, error) {
	return queryJSON[PracticeFinding](s.db, `SELECT data FROM simulation_findings WHERE run_id=? ORDER BY position`, runID)
}

// PracticeRunGet returns a run with every finding recorded so far.
func (s *Store) PracticeRunGet(id string) (PracticeRun, error) {
	run, err := s.practiceRunRow(id)
	if err != nil {
		return run, err
	}
	run.Findings, err = s.practiceFindings(id)
	return run, err
}

// PracticeHistory lists a responsibility's runs, newest first, without findings.
func (s *Store) PracticeHistory(responsibilityID string) ([]PracticeRun, error) {
	return queryJSON[PracticeRun](s.db, `SELECT data FROM simulation_runs WHERE responsibility_id=? ORDER BY rowid DESC LIMIT 100`, responsibilityID)
}

// FinishPracticeRun ends a run. With status "" the findings decide; any other
// status (mock_unfit, error, budget) ends it without counting it as a pass.
// It never enables anything: a person does, after reviewing the evidence.
func (s *Store) FinishPracticeRun(runID, status, note string) (PracticeRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, err := s.practiceRunRow(runID)
	if err != nil {
		return run, err
	}
	if run.Status != "running" {
		return run, ErrConflict
	}
	su, err := s.Suite(run.SuiteID)
	if err != nil {
		return run, err
	}
	findings, err := s.practiceFindings(runID)
	if err != nil {
		return run, err
	}
	r, err := s.GetPractice(run.ResponsibilityID)
	if err != nil {
		return run, err
	}
	run.EndedAt, run.Note, run.Current = s.now(), note, ""
	if status == "" {
		status = "failed"
		if PracticeRunPassed(su.Cases, findings) {
			status = "passed"
		}
	}
	run.Status = status
	if r.EvaluationID == run.ID && r.Revision == run.Revision && r.Readiness == Testing {
		r.Readiness, r.Authorization = NeedsRetesting, Supervised
		switch status {
		case "passed":
			r.Readiness, r.QualifiedAt, r.Reason = Qualified, s.now(), "Every practice case passed independent checks. A person must still enable this scope."
		case "failed":
			r.Reason = "One or more practice cases failed; review the evidence."
		default:
			r.Reason = "The practice run did not finish (" + status + "); it does not count. " + note
		}
	}
	r.UpdatedAt = s.now()
	run.Findings = nil
	b, err := json.Marshal(run)
	if err != nil {
		return run, err
	}
	rb, err := json.Marshal(r)
	if err != nil {
		return run, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return run, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE simulation_runs SET status=?,data=? WHERE id=?`, run.Status, string(b), run.ID); err != nil {
		return run, err
	}
	if _, err := tx.Exec(`UPDATE simulation_responsibilities SET data=? WHERE id=?`, string(rb), r.ID); err != nil {
		return run, err
	}
	if _, err := tx.Exec(`INSERT INTO qualification_events(responsibility_id,at_ns,actor,action,reason) VALUES(?,?,?,?,?)`, r.ID, sqlitedb.ToNS(s.now()), "verifier", run.Status, r.Reason); err != nil {
		return run, err
	}
	if err := tx.Commit(); err != nil {
		return run, err
	}
	run.Findings = findings
	return run, nil
}

// CancelPracticeRun records the cancellation durably before the runner stops.
func (s *Store) CancelPracticeRun(runID string) (PracticeRun, error) {
	return s.FinishPracticeRun(runID, "cancelled", "The owner cancelled the run.")
}

// PracticeFiles lists a run's evidence files without their bytes.
func (s *Store) PracticeFiles(runID string) ([]PracticeFile, error) {
	return sqlitedb.QueryAll(s.db, func(row sqlitedb.Scanner) (PracticeFile, error) {
		var f PracticeFile
		err := row.Scan(&f.CaseID, &f.Role, &f.Name, &f.SHA256, &f.Size)
		return f, err
	}, `SELECT case_id,role,name,sha256,LENGTH(body) FROM simulation_files WHERE run_id=? ORDER BY case_id,role,name`, runID)
}

// PracticeFileBody returns one evidence file with its bytes.
func (s *Store) PracticeFileBody(runID, caseID, role, name string) (PracticeFile, error) {
	f := PracticeFile{CaseID: caseID, Role: role, Name: name}
	err := s.db.QueryRow(`SELECT sha256,body FROM simulation_files WHERE run_id=? AND case_id=? AND role=? AND name=?`, runID, caseID, role, name).Scan(&f.SHA256, &f.Body)
	if err == sql.ErrNoRows {
		return f, ErrNotFound
	}
	f.Size = len(f.Body)
	return f, err
}

// AuthorizePractice enables only the current revision after a clean practice
// run, or suspends. Enabling lets the skill start unattended; it never relaxes
// the one-action approval for sending, paying or publishing.
func (s *Store) AuthorizePractice(id, revision, action string) (PracticeResponsibility, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.GetPractice(id)
	if err != nil {
		return r, err
	}
	if err := s.idle("simulation_runs", id); err != nil {
		return r, err
	}
	switch action {
	case "enable":
		if r.Revision == "" || r.Revision != revision || r.Readiness != Qualified || r.EvaluationID == "" {
			return r, ErrDenied
		}
		run, err := s.practiceRunRow(r.EvaluationID)
		if err != nil {
			return r, err
		}
		if run.Status != "passed" || run.Revision != r.Revision {
			return r, ErrDenied
		}
		r.Authorization, r.Reason = Enabled, "Owner enabled unattended starts of this revision. Sending, paying and publishing still ask every time."
	case "suspend":
		r.Authorization, r.Reason = Suspended, "Owner suspended this responsibility."
	default:
		return r, ErrInvalid
	}
	return r, s.savePractice(r, "owner", action, r.Reason)
}

// InvalidatePractice revokes a grant when its inputs or observed results change.
func (s *Store) InvalidatePractice(id, reason string, suspend bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.GetPractice(id)
	if err != nil {
		return err
	}
	r.Readiness, r.Authorization, r.Reason = NeedsRetesting, Supervised, reason
	if suspend {
		r.Authorization = Suspended
	}
	return s.savePractice(r, "control_plane", "invalidate", reason)
}

// PracticeGrant returns the skill's responsibility and whether it is enabled
// for unattended starts right now.
func (s *Store) PracticeGrant(worker, skill string) (PracticeResponsibility, bool, error) {
	r, err := s.practiceFor(worker, SourceSkill, skill)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	return r, r.Readiness == Qualified && r.Authorization == Enabled, nil
}

// PracticeTaskFailed suspends a skill's grant after a production failure.
// Retracting the verdict later does not restore it.
func (s *Store) PracticeTaskFailed(worker, skill, reason string) error {
	r, err := s.practiceFor(worker, SourceSkill, skill)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if r.Authorization != Enabled && r.Readiness != Qualified {
		return nil
	}
	return s.InvalidatePractice(r.ID, reason, true)
}

// HoldUnattended records an unattended start waiting for a person, once per
// worker and request key; a repeat returns the hold already there.
func (s *Store) HoldUnattended(worker, skill, request, key string) (PracticeHold, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h, err := s.holdByKey(worker, key); err == nil {
		return h, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return h, err
	}
	h := PracticeHold{ID: sqlitedb.NewID("h", 16), WorkerID: worker, Skill: skill, Request: request, Status: HoldWaiting, CreatedAt: s.now()}
	if len(h.Request) > 4000 {
		h.Request = h.Request[:4000]
	}
	b, err := json.Marshal(h)
	if err != nil {
		return h, err
	}
	_, err = s.db.Exec(`INSERT INTO simulation_holds(id,worker_id,request_key,status,data) VALUES(?,?,?,?,?)`, h.ID, worker, key, h.Status, string(b))
	return h, err
}

func (s *Store) holdByKey(worker, key string) (PracticeHold, error) {
	return scanHold(s.db.QueryRow(`SELECT status,data FROM simulation_holds WHERE worker_id=? AND request_key=?`, worker, key))
}

// scanHold reads a hold's (status, data) row; the status column is its current state.
func scanHold(row sqlitedb.Scanner) (PracticeHold, error) {
	var b, status string
	if err := row.Scan(&status, &b); err != nil {
		return PracticeHold{}, err
	}
	var h PracticeHold
	err := json.Unmarshal([]byte(b), &h)
	h.Status = status
	return h, err
}

// TakeHold returns the hold for this request. A confirmed or declined hold is
// used up by reading it: the next unattended start of it asks again.
func (s *Store) TakeHold(worker, key string) (PracticeHold, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, err := s.holdByKey(worker, key)
	if errors.Is(err, sql.ErrNoRows) {
		return h, false, nil
	}
	if err != nil {
		return h, false, err
	}
	if h.Status != HoldWaiting {
		if _, err := s.db.Exec(`DELETE FROM simulation_holds WHERE id=?`, h.ID); err != nil {
			return h, false, err
		}
	}
	return h, true, nil
}

// SettleHold records a person's confirm or decline of a waiting hold.
func (s *Store) SettleHold(id, status string) (PracticeHold, error) {
	if status != HoldConfirmed && status != HoldDeclined {
		return PracticeHold{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var b, current string
	err := s.db.QueryRow(`SELECT status,data FROM simulation_holds WHERE id=?`, id).Scan(&current, &b)
	if err == sql.ErrNoRows {
		return PracticeHold{}, ErrNotFound
	}
	if err != nil {
		return PracticeHold{}, err
	}
	var h PracticeHold
	if err := json.Unmarshal([]byte(b), &h); err != nil {
		return h, err
	}
	if current != HoldWaiting {
		return h, ErrConflict
	}
	h.Status = status
	nb, err := json.Marshal(h)
	if err != nil {
		return h, err
	}
	_, err = s.db.Exec(`UPDATE simulation_holds SET status=?,data=? WHERE id=?`, status, string(nb), id)
	return h, err
}

// Holds lists the unattended starts waiting for a person ("" for every worker).
func (s *Store) Holds(worker string) ([]PracticeHold, error) {
	return sqlitedb.QueryAll(s.db, scanHold, `SELECT status,data FROM simulation_holds WHERE (?='' OR worker_id=?) ORDER BY rowid`, worker, worker)
}
