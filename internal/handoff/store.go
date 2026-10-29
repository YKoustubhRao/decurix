// Package handoff lets one worker hand a typed piece of work to another
// worker - or ask another worker a question - through the control plane,
// and get back a result that says whether the receiving worker's own final
// gate confirmed it. A result the receiver did not verify is never "done".
//
// It imports nothing of internal/httpapi: task submission, task outcomes and
// worker lookup arrive as small interfaces (service.go) that httpapi adapts.
package handoff

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// Statuses a handoff moves through. queued and running are open; the rest
// are final.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusDone      = "done"
	StatusNotDone   = "not_done"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// Kinds of handoff: a piece of work, or a question put to a worker taught a skill.
const (
	KindDelegate = "delegate"
	KindAsk      = "ask"
)

// ErrNotFound is returned for a handoff that does not exist.
var ErrNotFound = errors.New("handoff: not found")

// Result is what came back from the receiving worker. Verified is true only
// when the receiver's final gate checked the result and called it done.
type Result struct {
	Report        string   `json:"report"`
	Verified      bool     `json:"verified"`
	Evidence      []string `json:"evidence"`
	Files         []string `json:"files"`
	OpenQuestions []string `json:"open_questions"`
}

// Handoff is one piece of work (or one question) handed from a worker to another.
type Handoff struct {
	ID         string            `json:"id"`
	Kind       string            `json:"kind"`
	FromWorker string            `json:"from_worker"`
	FromTask   string            `json:"from_task"`
	ToWorker   string            `json:"to_worker"`
	ToRole     string            `json:"to_role"`
	ToSkill    string            `json:"to_skill"`
	Request    string            `json:"request"`
	Inputs     map[string]string `json:"inputs"`
	Files      []string          `json:"files"`
	Expected   string            `json:"expected"`
	Depth      int               `json:"depth"`
	Status     string            `json:"status"`
	// Reason says, in words a worker can act on, why the handoff is where it
	// is: queued behind a busy worker, failed for want of one, not verified.
	Reason    string    `json:"reason"`
	TaskID    string    `json:"task_id"`
	Result    Result    `json:"result"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// DispatchedAt is when the receiving task was submitted (zero while queued).
	DispatchedAt time.Time `json:"dispatched_at"`
}

// Open reports whether the handoff can still change on its own.
func (h Handoff) Open() bool {
	return h.Status == StatusQueued || h.Status == StatusRunning
}

// migrations are the handoff component's schema steps: append-only.
var migrations = []string{
	`CREATE TABLE handoffs (
		id            TEXT PRIMARY KEY,
		kind          TEXT NOT NULL,
		from_worker   TEXT NOT NULL,
		from_task     TEXT NOT NULL DEFAULT '',
		to_worker     TEXT NOT NULL DEFAULT '',
		to_role       TEXT NOT NULL DEFAULT '',
		to_skill      TEXT NOT NULL DEFAULT '',
		request       TEXT NOT NULL,
		inputs        TEXT NOT NULL DEFAULT '{}',
		files         TEXT NOT NULL DEFAULT '[]',
		expected      TEXT NOT NULL DEFAULT '',
		depth         INTEGER NOT NULL DEFAULT 0,
		status        TEXT NOT NULL,
		reason        TEXT NOT NULL DEFAULT '',
		task_id       TEXT NOT NULL DEFAULT '',
		result        TEXT NOT NULL DEFAULT '{}',
		created_ns    INTEGER NOT NULL,
		updated_ns    INTEGER NOT NULL,
		dispatched_ns INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX handoffs_status ON handoffs (status)`,
	`CREATE INDEX handoffs_task ON handoffs (task_id)`,
	`CREATE INDEX handoffs_from ON handoffs (from_worker, created_ns)`,
	`CREATE INDEX handoffs_to ON handoffs (to_worker, created_ns)`,
}

const columns = `id, kind, from_worker, from_task, to_worker, to_role, to_skill, request, inputs, files, expected,
	depth, status, reason, task_id, result, created_ns, updated_ns, dispatched_ns`

// Store keeps handoffs in SQLite.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// NewStore migrates the handoff schema on db and returns its store.
func NewStore(db *sql.DB) (*Store, error) {
	if err := sqlitedb.Migrate(db, "handoff", migrations); err != nil {
		return nil, err
	}

	return &Store{db: db, now: func() time.Time { return time.Now().UTC() }}, nil
}

// scan reads one handoff row.
func scan(row sqlitedb.Scanner) (Handoff, error) {
	var h Handoff
	var inputs, files, result string
	var created, updated, dispatched int64

	if err := row.Scan(&h.ID, &h.Kind, &h.FromWorker, &h.FromTask, &h.ToWorker, &h.ToRole, &h.ToSkill,
		&h.Request, &inputs, &files, &h.Expected, &h.Depth, &h.Status, &h.Reason, &h.TaskID, &result,
		&created, &updated, &dispatched); err != nil {
		return Handoff{}, err
	}

	// A column that does not parse reads as empty: the row is still shown.
	_ = json.Unmarshal([]byte(inputs), &h.Inputs)
	_ = json.Unmarshal([]byte(files), &h.Files)
	_ = json.Unmarshal([]byte(result), &h.Result)
	if h.Inputs == nil {
		h.Inputs = map[string]string{}
	}
	if h.Files == nil {
		h.Files = []string{}
	}
	h.Result = normalResult(h.Result)
	h.CreatedAt, h.UpdatedAt, h.DispatchedAt = sqlitedb.FromNS(created), sqlitedb.FromNS(updated), sqlitedb.FromNS(dispatched)

	return h, nil
}

// normalResult makes a result's lists non-nil, so JSON shows [] not null.
func normalResult(r Result) Result {
	for _, list := range []*[]string{&r.Evidence, &r.Files, &r.OpenQuestions} {
		if *list == nil {
			*list = []string{}
		}
	}

	return r
}

// Create stores a new handoff and returns it with its ID and times set.
func (s *Store) Create(h Handoff) (Handoff, error) {
	now := s.now()
	h.ID = sqlitedb.NewID("ho_", 8)
	h.CreatedAt, h.UpdatedAt = now, now
	if h.Status == "" {
		h.Status = StatusQueued
	}
	if h.Inputs == nil {
		h.Inputs = map[string]string{}
	}
	if h.Files == nil {
		h.Files = []string{}
	}
	h.Result = normalResult(h.Result)

	inputs, _ := json.Marshal(h.Inputs)
	files, _ := json.Marshal(h.Files)
	result, _ := json.Marshal(h.Result)

	if _, err := s.db.Exec(`INSERT INTO handoffs (`+columns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		h.ID, h.Kind, h.FromWorker, h.FromTask, h.ToWorker, h.ToRole, h.ToSkill, h.Request, string(inputs),
		string(files), h.Expected, h.Depth, h.Status, h.Reason, h.TaskID, string(result),
		sqlitedb.ToNS(h.CreatedAt), sqlitedb.ToNS(h.UpdatedAt), sqlitedb.ToNS(h.DispatchedAt)); err != nil {
		return Handoff{}, fmt.Errorf("handoff: create: %w", err)
	}

	return h, nil
}

// Get returns one handoff, or ErrNotFound.
func (s *Store) Get(id string) (Handoff, error) {
	h, err := scan(s.db.QueryRow(`SELECT `+columns+` FROM handoffs WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return Handoff{}, ErrNotFound
	}
	if err != nil {
		return Handoff{}, fmt.Errorf("handoff: get: %w", err)
	}

	return h, nil
}

// ByTask returns the newest handoff whose receiving task is taskID, or ErrNotFound.
func (s *Store) ByTask(taskID string) (Handoff, error) {
	if strings.TrimSpace(taskID) == "" {
		return Handoff{}, ErrNotFound
	}

	h, err := scan(s.db.QueryRow(`SELECT `+columns+` FROM handoffs WHERE task_id = ? ORDER BY created_ns DESC LIMIT 1`, taskID))
	if err == sql.ErrNoRows {
		return Handoff{}, ErrNotFound
	}
	if err != nil {
		return Handoff{}, fmt.Errorf("handoff: by task: %w", err)
	}

	return h, nil
}

// List returns the newest handoffs a worker sent or received (every worker's
// when worker is ""), at most limit.
func (s *Store) List(worker string, limit int) ([]Handoff, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	if worker == "" {
		return sqlitedb.QueryAll(s.db, scan, `SELECT `+columns+` FROM handoffs ORDER BY created_ns DESC LIMIT ?`, limit)
	}

	return sqlitedb.QueryAll(s.db, scan,
		`SELECT `+columns+` FROM handoffs WHERE from_worker = ? OR to_worker = ? ORDER BY created_ns DESC LIMIT ?`,
		worker, worker, limit)
}

// OpenHandoffs returns every queued or running handoff, oldest first.
func (s *Store) OpenHandoffs() ([]Handoff, error) {
	return sqlitedb.QueryAll(s.db, scan,
		`SELECT `+columns+` FROM handoffs WHERE status IN (?, ?) ORDER BY created_ns`, StatusQueued, StatusRunning)
}

// Save writes back a handoff's changing fields (where it went, its status,
// reason, task and result), stamping UpdatedAt. Only an open row is written:
// a final handoff never changes again, and false says the row was final.
func (s *Store) Save(h Handoff) (Handoff, bool, error) {
	h.UpdatedAt = s.now()
	h.Result = normalResult(h.Result)
	result, _ := json.Marshal(h.Result)

	res, err := s.db.Exec(`UPDATE handoffs SET to_worker = ?, status = ?, reason = ?, task_id = ?, result = ?,
		updated_ns = ?, dispatched_ns = ? WHERE id = ? AND status IN (?, ?)`,
		h.ToWorker, h.Status, h.Reason, h.TaskID, string(result), sqlitedb.ToNS(h.UpdatedAt),
		sqlitedb.ToNS(h.DispatchedAt), h.ID, StatusQueued, StatusRunning)
	if err != nil {
		return Handoff{}, false, fmt.Errorf("handoff: save: %w", err)
	}

	n, _ := res.RowsAffected()
	return h, n == 1, nil
}
