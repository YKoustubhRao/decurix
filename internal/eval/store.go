package eval

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// Run states.
const (
	RunRunning     = "running"
	RunDone        = "done"
	RunCancelled   = "cancelled"
	RunError       = "error"
	RunInterrupted = "interrupted"
)

// schema is the eval component's Migrate slice: append a step, never edit one.
var schema = []string{
	`CREATE TABLE eval_runs (
		id TEXT PRIMARY KEY,
		created_ns INTEGER NOT NULL,
		suite_name TEXT NOT NULL,
		suite_sha TEXT NOT NULL,
		suite_blob TEXT NOT NULL DEFAULT '',
		arms TEXT NOT NULL,
		cases TEXT NOT NULL DEFAULT '[]',
		repeats INTEGER NOT NULL,
		status TEXT NOT NULL,
		note TEXT NOT NULL DEFAULT '',
		started_ns INTEGER NOT NULL DEFAULT 0,
		ended_ns INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE eval_rows (
		run_id TEXT NOT NULL,
		arm TEXT NOT NULL,
		repeat INTEGER NOT NULL,
		case_id TEXT NOT NULL,
		task_id TEXT NOT NULL DEFAULT '',
		observed TEXT NOT NULL,
		checked INTEGER NOT NULL DEFAULT 0,
		passed INTEGER NOT NULL,
		false_done INTEGER NOT NULL,
		done_unchecked_wrong INTEGER NOT NULL,
		q_required INTEGER NOT NULL DEFAULT 0,
		q_other INTEGER NOT NULL DEFAULT 0,
		tokens_in INTEGER NOT NULL DEFAULT 0,
		tokens_cached INTEGER NOT NULL DEFAULT 0,
		tokens_out INTEGER NOT NULL DEFAULT 0,
		minutes REAL NOT NULL DEFAULT 0,
		deployments TEXT NOT NULL DEFAULT '[]',
		checks TEXT NOT NULL DEFAULT '{}',
		problems TEXT NOT NULL DEFAULT '[]',
		at_ns INTEGER NOT NULL
	);
	CREATE INDEX eval_rows_run ON eval_rows(run_id, arm, repeat);`,
}

// Blobs keeps a run's suite tar by content (*blobstore.Store), so a run can be read again later.
type Blobs interface {
	PutBytes(b []byte) (string, error)
	Open(sha string) (io.ReadCloser, error)
}

// Run is one matrix run: a suite, its arms, the repeats.
type Run struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	SuiteName string    `json:"suite_name"`
	SuiteSHA  string    `json:"suite_sha"`
	SuiteBlob string    `json:"suite_blob,omitempty"`
	Arms      []Arm     `json:"arms"`
	Cases     []string  `json:"cases"`
	Repeats   int       `json:"repeats"`
	Status    string    `json:"status"`
	Note      string    `json:"note,omitempty"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
}

// Row is one case × arm × repeat. Rows are only ever inserted (plan rule 4).
type Row struct {
	RunID              string          `json:"run_id"`
	Arm                string          `json:"arm"`
	Repeat             int             `json:"repeat"`
	CaseID             string          `json:"case_id"`
	TaskID             string          `json:"task_id"`
	Observed           string          `json:"observed"`
	Checked            bool            `json:"checked"`
	Passed             bool            `json:"passed"`
	FalseDone          bool            `json:"false_done"`
	DoneUncheckedWrong bool            `json:"done_unchecked_wrong"`
	QRequired          int             `json:"q_required"`
	QOther             int             `json:"q_other"`
	TokensIn           int64           `json:"tokens_in"`
	TokensCached       int64           `json:"tokens_cached"`
	TokensOut          int64           `json:"tokens_out"`
	Minutes            float64         `json:"minutes"`
	Deployments        []string        `json:"deployments"`
	Checks             json.RawMessage `json:"checks"`
	Problems           []string        `json:"problems"`
	At                 time.Time       `json:"at"`
}

// Invalid reports whether the row's model changed under it (D10).
func (r Row) Invalid() bool {
	for _, p := range r.Problems {
		if p == modelChanged {
			return true
		}
	}
	return false
}

// Store keeps eval runs and their rows in SQLite.
type Store struct {
	db    *sql.DB
	blobs Blobs
	mu    sync.Mutex
	now   func() time.Time
}

// ErrNotFound is a run that does not exist.
var ErrNotFound = errors.New("eval: not found")

// NewStore migrates the eval tables and marks a run left running by a previous control plane interrupted; its rows
// stand.
func NewStore(db *sql.DB, blobs Blobs) (*Store, error) {
	if err := sqlitedb.Migrate(db, "eval", schema); err != nil {
		return nil, err
	}
	s := &Store{db: db, blobs: blobs, now: func() time.Time { return time.Now().UTC() }}
	if _, err := db.Exec(`UPDATE eval_runs SET status=?, note=CASE WHEN note='' THEN ? ELSE note || ' ' || ? END, ended_ns=? WHERE status=?`,
		RunInterrupted, interruptedNote, interruptedNote, sqlitedb.ToNS(s.now()), RunRunning); err != nil {
		return nil, err
	}
	return s, nil
}

const interruptedNote = "The control plane restarted during the run; the rows stored before it stand."

// PutSuite keeps a suite tar as a blob and returns its SHA ("" when no blob store is set).
func (s *Store) PutSuite(tar []byte) (string, error) {
	if s.blobs == nil {
		return "", nil
	}
	return s.blobs.PutBytes(tar)
}

// SuiteTar reads a run's suite tar back from the blob store.
func (s *Store) SuiteTar(sha string) ([]byte, error) {
	if s.blobs == nil || sha == "" {
		return nil, ErrNotFound
	}
	rc, err := s.blobs.Open(sha)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, io.LimitReader(rc, MaxSuiteBytes+1)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Create stores a new run as running. It refuses while another run is running (the store's own lock; the practice
// slot is the process's).
func (s *Store) Create(r Run) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM eval_runs WHERE status=?`, RunRunning).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrBusy
	}
	arms, err := json.Marshal(r.Arms)
	if err != nil {
		return err
	}
	cases, _ := json.Marshal(r.Cases)
	_, err = s.db.Exec(`INSERT INTO eval_runs (id, created_ns, suite_name, suite_sha, suite_blob, arms, cases, repeats, status, note, started_ns, ended_ns)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`,
		r.ID, sqlitedb.ToNS(r.CreatedAt), r.SuiteName, r.SuiteSHA, r.SuiteBlob, string(arms), string(cases), r.Repeats, RunRunning, r.Note,
		sqlitedb.ToNS(r.StartedAt))
	return err
}

// SetArms stores the arms as pinned at their start (image, deployment, tree).
func (s *Store) SetArms(id string, arms []Arm) error {
	b, err := json.Marshal(arms)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE eval_runs SET arms=? WHERE id=?`, string(b), id)
	return err
}

// Finish ends a running run with a status and a note; a run already ended keeps its first ending (a cancel then a
// late finish says cancelled).
func (s *Store) Finish(id, status, note string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE eval_runs SET status=?, note=CASE WHEN note='' THEN ? WHEN ?='' THEN note ELSE note || ' ' || ? END, ended_ns=?
		WHERE id=? AND status=?`, status, note, note, note, sqlitedb.ToNS(s.now()), id, RunRunning)
	return err
}

// AddNote appends a sentence to a run's note.
func (s *Store) AddNote(id, note string) error {
	_, err := s.db.Exec(`UPDATE eval_runs SET note=CASE WHEN note='' THEN ? ELSE note || ' ' || ? END WHERE id=?`, note, note, id)
	return err
}

// AddRow inserts one row. Rows are never updated or deleted.
func (s *Store) AddRow(r Row) error {
	deps, _ := json.Marshal(nonNil(r.Deployments))
	probs, _ := json.Marshal(nonNil(r.Problems))
	checks := r.Checks
	if len(checks) == 0 {
		checks = json.RawMessage(`{}`)
	}
	_, err := s.db.Exec(`INSERT INTO eval_rows (run_id, arm, repeat, case_id, task_id, observed, checked, passed, false_done,
		done_unchecked_wrong, q_required, q_other, tokens_in, tokens_cached, tokens_out, minutes, deployments, checks, problems, at_ns)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.RunID, r.Arm, r.Repeat, r.CaseID, r.TaskID, r.Observed, r.Checked, r.Passed, r.FalseDone, r.DoneUncheckedWrong,
		r.QRequired, r.QOther, r.TokensIn, r.TokensCached, r.TokensOut, r.Minutes, string(deps), string(checks), string(probs),
		sqlitedb.ToNS(r.At))
	return err
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Get returns one run.
func (s *Store) Get(id string) (Run, error) {
	runs, err := s.query(`WHERE id=?`, id)
	if err != nil {
		return Run{}, err
	}
	if len(runs) == 0 {
		return Run{}, ErrNotFound
	}
	return runs[0], nil
}

// List returns the latest runs, newest first.
func (s *Store) List(limit int) ([]Run, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	return s.query(`ORDER BY created_ns DESC LIMIT ?`, limit)
}

func (s *Store) query(where string, args ...any) ([]Run, error) {
	rows, err := s.db.Query(`SELECT id, created_ns, suite_name, suite_sha, suite_blob, arms, cases, repeats, status, note, started_ns, ended_ns
		FROM eval_runs `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var created, started, ended int64
		var arms, cases string
		if err := rows.Scan(&r.ID, &created, &r.SuiteName, &r.SuiteSHA, &r.SuiteBlob, &arms, &cases, &r.Repeats, &r.Status, &r.Note, &started, &ended); err != nil {
			return nil, err
		}
		r.CreatedAt, r.StartedAt = sqlitedb.FromNS(created), sqlitedb.FromNS(started)
		if ended > 0 {
			r.EndedAt = sqlitedb.FromNS(ended)
		}
		_ = json.Unmarshal([]byte(arms), &r.Arms)
		_ = json.Unmarshal([]byte(cases), &r.Cases)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Rows returns a run's rows in the order they were stored.
func (s *Store) Rows(runID string) ([]Row, error) {
	rows, err := s.db.Query(`SELECT run_id, arm, repeat, case_id, task_id, observed, checked, passed, false_done, done_unchecked_wrong,
		q_required, q_other, tokens_in, tokens_cached, tokens_out, minutes, deployments, checks, problems, at_ns
		FROM eval_rows WHERE run_id=? ORDER BY rowid`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Row{}
	for rows.Next() {
		var r Row
		var deps, checks, probs string
		var at int64
		if err := rows.Scan(&r.RunID, &r.Arm, &r.Repeat, &r.CaseID, &r.TaskID, &r.Observed, &r.Checked, &r.Passed, &r.FalseDone,
			&r.DoneUncheckedWrong, &r.QRequired, &r.QOther, &r.TokensIn, &r.TokensCached, &r.TokensOut, &r.Minutes, &deps, &checks, &probs, &at); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(deps), &r.Deployments)
		_ = json.Unmarshal([]byte(probs), &r.Problems)
		r.Checks = json.RawMessage(checks)
		r.At = sqlitedb.FromNS(at)
		out = append(out, r)
	}
	return out, rows.Err()
}
