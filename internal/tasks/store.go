// Package tasks turns chat requests into durable records: a Task (what a human
// asked a worker for) has Executions (attempts at it), each with the Steps the
// agent reported, the Interventions where it stopped for a human and the reply
// it got, and an Outcome - done, query or error - with the time it took and
// what it cost. A human can then mark the answer correct or wrong, since "done"
// on its own is only the agent's claim.
//
// Records live in the control plane's SQLite database, keyed by worker (the
// desktop ID), so a worker's history outlives both a restart and its container.
// They are what the autonomy report (report.go) is computed from.
package tasks

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/YKoustubhRao/flowera/internal/sqlitedb"
)

// Execution statuses. queued, running and waiting are live; the rest are how
// an execution ended. "query" is an execution that ended while still waiting on
// a human - its desktop was stopped before anyone replied.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusWaiting   = "waiting"
	StatusDone      = "done"
	StatusError     = "error"
	StatusQuery     = "query"
	StatusAbandoned = "abandoned"
)

// Intervention kinds: the agent was stuck, or stopped to ask permission.
const (
	InterventionQuery    = "query"
	InterventionApproval = "approval"
)

// Verdicts a human can give a finished task.
const (
	VerdictCorrect = "correct"
	VerdictWrong   = "wrong"
)

// Outcome kinds a desktop reports when a turn ends.
const (
	OutcomeDone     = "done"
	OutcomeError    = "error"
	OutcomeQuery    = "query"
	OutcomeApproval = "approval"
)

// lateWindow is how long after an execution ends a step for it is still
// recorded against it: a desktop's steps travel on a background queue and its
// reply does not, so a turn's last line can land after the reply that ended it.
const lateWindow = 10 * time.Second

// retryWindow bounds how long after a failure resending the same words counts
// as another attempt at the same task rather than a new one. A failure note
// tells the human to "send the request again"; that is a retry.
const retryWindow = time.Hour

// ErrNotFound is returned for a task that does not exist.
var ErrNotFound = errors.New("tasks: not found")

// Worker is a desktop as the records know it. It is kept after its container
// is deleted, so a worker's history does not vanish with it.
type Worker struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Engine    string    `json:"engine"`
	Profile   string    `json:"profile"`
	Model     string    `json:"model"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	DeletedAt time.Time `json:"deleted_at"`
}

// Task is one thing a human asked a worker to do.
type Task struct {
	ID          string    `json:"id"`
	WorkerID    string    `json:"worker_id"`
	WorkerName  string    `json:"worker_name"`
	Engine      string    `json:"engine"`
	Request     string    `json:"request"`
	Type        string    `json:"type"`
	Status      string    `json:"status"`
	Verdict     string    `json:"verdict"`
	VerdictNote string    `json:"verdict_note"`
	VerdictAt   time.Time `json:"verdict_at"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Execution is one attempt at a task by one engine.
type Execution struct {
	ID            string         `json:"id"`
	TaskID        string         `json:"task_id"`
	Attempt       int            `json:"attempt"`
	WorkerID      string         `json:"worker_id"`
	Engine        string         `json:"engine"`
	Model         string         `json:"model"`
	Status        string         `json:"status"`
	FinalReply    string         `json:"final_reply"`
	StartedAt     time.Time      `json:"started_at"`
	EndedAt       time.Time      `json:"ended_at"`
	CostUSD       *float64       `json:"cost_usd"`
	InputTokens   *int64         `json:"input_tokens"`
	OutputTokens  *int64         `json:"output_tokens"`
	Steps         []Step         `json:"steps"`
	Interventions []Intervention `json:"interventions"`
}

// Step is one line of activity an execution reported. An on-screen action
// carries its Action, with the URLs of its before and after screenshots.
type Step struct {
	ID        string    `json:"id"`
	Text      string    `json:"text"`
	Action    *Action   `json:"action,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Action is one on-screen action, as reported by the desktop's action layer.
type Action struct {
	RecordID string `json:"record_id,omitempty"`
	Kind     string `json:"kind"`
	Target   string `json:"target,omitempty"`
	Backend  string `json:"backend,omitempty"`
	Outcome  string `json:"outcome,omitempty"`
	Change   string `json:"change,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Before   string `json:"before,omitempty"`
	After    string `json:"after,omitempty"`
}

// Intervention is one time an execution stopped for a human, and what the
// human said back.
type Intervention struct {
	ID          string    `json:"id"`
	ExecutionID string    `json:"execution_id"`
	Kind        string    `json:"kind"`
	Reason      string    `json:"reason"`
	Request     string    `json:"request"`
	Reply       string    `json:"reply"`
	RequestedAt time.Time `json:"requested_at"`
	RepliedAt   time.Time `json:"replied_at"`
}

// Event is one entry of an execution's timeline: a step or an intervention.
type Event struct {
	At           time.Time
	Step         *Step
	Intervention *Intervention
}

// Timeline merges an execution's steps and interventions in the order they
// happened, which is how a person reads what the agent did.
func (e Execution) Timeline() []Event {
	events := make([]Event, 0, len(e.Steps)+len(e.Interventions))

	for i := range e.Steps {
		events = append(events, Event{At: e.Steps[i].CreatedAt, Step: &e.Steps[i]})
	}

	for i := range e.Interventions {
		events = append(events, Event{At: e.Interventions[i].RequestedAt, Intervention: &e.Interventions[i]})
	}

	sort.SliceStable(events, func(i, j int) bool { return events[i].At.Before(events[j].At) })

	return events
}

// Duration is how long the execution has taken, or took.
func (e Execution) Duration(now time.Time) time.Duration {
	switch {
	case !e.EndedAt.IsZero():
		return e.EndedAt.Sub(e.StartedAt)
	case e.Status == StatusRunning || e.Status == StatusWaiting:
		return now.Sub(e.StartedAt)
	default:
		return 0
	}
}

// Detail is a task with everything recorded under it.
type Detail struct {
	Task
	Worker     Worker      `json:"worker"`
	Executions []Execution `json:"executions"`
}

// Outcome is how a desktop reports the end of a turn: its answer, a query or
// a request for approval, or a failure, plus whatever it knows about the model
// and what the task has cost so far. Nil or empty fields are left as they were.
type Outcome struct {
	Kind         string
	Reply        string
	Engine       string
	Model        string
	CostUSD      *float64
	InputTokens  *int64
	OutputTokens *int64
}

// Route is where a human request goes: a new task, the reply to a task's open
// intervention, or another attempt at a task that just failed.
type Route struct {
	Kind           string `json:"kind"`
	TaskID         string `json:"task_id"`
	ExecutionID    string `json:"execution_id"`
	InterventionID string `json:"intervention_id,omitempty"`
}

// Route kinds.
const (
	RouteNew   = "new"
	RouteReply = "reply"
	RouteRetry = "retry"
)

var schema = []string{
	`CREATE TABLE workers (
		id            TEXT PRIMARY KEY,
		name          TEXT NOT NULL,
		engine        TEXT NOT NULL DEFAULT '',
		profile       TEXT NOT NULL DEFAULT '',
		model         TEXT NOT NULL DEFAULT '',
		first_seen_ns INTEGER NOT NULL,
		last_seen_ns  INTEGER NOT NULL,
		deleted_ns    INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE TABLE tasks (
		id           TEXT PRIMARY KEY,
		worker_id    TEXT NOT NULL,
		engine       TEXT NOT NULL DEFAULT '',
		request      TEXT NOT NULL,
		task_type    TEXT NOT NULL,
		status       TEXT NOT NULL,
		verdict      TEXT NOT NULL DEFAULT '',
		verdict_note TEXT NOT NULL DEFAULT '',
		verdict_ns   INTEGER NOT NULL DEFAULT 0,
		created_ns   INTEGER NOT NULL,
		updated_ns   INTEGER NOT NULL
	)`,
	`CREATE INDEX tasks_created ON tasks (created_ns)`,
	`CREATE INDEX tasks_worker ON tasks (worker_id, created_ns)`,
	`CREATE TABLE executions (
		id            TEXT PRIMARY KEY,
		task_id       TEXT NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
		attempt       INTEGER NOT NULL,
		worker_id     TEXT NOT NULL,
		engine        TEXT NOT NULL DEFAULT '',
		model         TEXT NOT NULL DEFAULT '',
		status        TEXT NOT NULL,
		final_message TEXT NOT NULL DEFAULT '',
		started_ns    INTEGER NOT NULL,
		ended_ns      INTEGER NOT NULL DEFAULT 0,
		cost_usd      REAL,
		input_tokens  INTEGER,
		output_tokens INTEGER
	)`,
	`CREATE INDEX executions_task ON executions (task_id, attempt)`,
	`CREATE INDEX executions_worker ON executions (worker_id, status)`,
	`CREATE TABLE execution_steps (
		id           TEXT PRIMARY KEY,
		execution_id TEXT NOT NULL REFERENCES executions (id) ON DELETE CASCADE,
		text         TEXT NOT NULL,
		action       TEXT NOT NULL DEFAULT '',
		created_ns   INTEGER NOT NULL
	)`,
	`CREATE INDEX execution_steps_execution ON execution_steps (execution_id)`,
	`CREATE TABLE interventions (
		id           TEXT PRIMARY KEY,
		execution_id TEXT NOT NULL REFERENCES executions (id) ON DELETE CASCADE,
		kind         TEXT NOT NULL,
		reason       TEXT NOT NULL,
		request      TEXT NOT NULL,
		reply        TEXT NOT NULL DEFAULT '',
		requested_ns INTEGER NOT NULL,
		replied_ns   INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX interventions_execution ON interventions (execution_id)`,
	// The steps above are applied on every existing installation and must
	// never be edited - schema changes only ever append.
	`ALTER TABLE executions RENAME COLUMN final_message TO final_reply`,
}

// Store is the SQLite-backed record of every worker's tasks.
type Store struct {
	db *sql.DB

	// mu serialises the store's read-decide-write sequences (routing a
	// request, closing one execution and promoting the next), which each span
	// several statements.
	mu sync.Mutex

	now func() time.Time
}

// NewStore opens the task Store on db, creating its tables if needed.
func NewStore(db *sql.DB) (*Store, error) {
	if err := sqlitedb.Migrate(db, "tasks", schema); err != nil {
		return nil, err
	}

	return &Store{db: db, now: func() time.Time { return time.Now().UTC() }}, nil
}

// newID returns a random identifier with a one-letter prefix naming what it is.
// Task IDs travel into a desktop inside a file name, so they stay [a-z0-9].
func newID(prefix string) string {
	raw := make([]byte, 8)

	if _, err := rand.Read(raw); err != nil {
		panic(fmt.Sprintf("tasks: generating id: %v", err))
	}

	return prefix + hex.EncodeToString(raw)
}

// ValidID reports whether id has the shape of an ID this store issues, which
// is how a value reported back by a desktop is checked before it is used.
func ValidID(id string) bool {
	if len(id) != 17 {
		return false
	}

	for _, r := range id[1:] {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}

	return true
}

// ---------------------------------------------------------------------------
// workers
// ---------------------------------------------------------------------------

// UpsertWorker records a worker, or refreshes what is known about it. Empty
// fields never overwrite known ones.
func (s *Store) UpsertWorker(worker Worker) error {
	if worker.ID == "" {
		return errors.New("tasks: worker id is required")
	}

	now := sqlitedb.ToNS(s.now())

	if worker.Name == "" {
		worker.Name = worker.ID
	}

	_, err := s.db.Exec(
		`INSERT INTO workers (id, name, engine, profile, model, first_seen_ns, last_seen_ns)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			engine = CASE WHEN excluded.engine != '' THEN excluded.engine ELSE workers.engine END,
			profile = CASE WHEN excluded.profile != '' THEN excluded.profile ELSE workers.profile END,
			model = CASE WHEN excluded.model != '' THEN excluded.model ELSE workers.model END,
			last_seen_ns = excluded.last_seen_ns,
			deleted_ns = 0`,
		worker.ID, worker.Name, worker.Engine, worker.Profile, worker.Model, now, now,
	)
	if err != nil {
		return fmt.Errorf("tasks: saving worker %s: %w", worker.ID, err)
	}

	return nil
}

// Worker returns one worker's record.
func (s *Store) Worker(id string) (Worker, bool) {
	var worker Worker
	var first, last, deleted int64

	err := s.db.QueryRow(
		`SELECT id, name, engine, profile, model, first_seen_ns, last_seen_ns, deleted_ns FROM workers WHERE id = ?`, id,
	).Scan(&worker.ID, &worker.Name, &worker.Engine, &worker.Profile, &worker.Model, &first, &last, &deleted)
	if err != nil {
		return Worker{}, false
	}

	worker.FirstSeen = sqlitedb.FromNS(first)
	worker.LastSeen = sqlitedb.FromNS(last)
	worker.DeletedAt = sqlitedb.FromNS(deleted)

	return worker, true
}

// MarkWorkerDeleted notes that a worker's container is gone. Its records stay.
func (s *Store) MarkWorkerDeleted(id string) error {
	_, err := s.db.Exec(`UPDATE workers SET deleted_ns = ? WHERE id = ?`, sqlitedb.ToNS(s.now()), id)
	return err
}

// ---------------------------------------------------------------------------
// human requests
// ---------------------------------------------------------------------------

// Plan decides where a human request to a worker belongs, without recording
// anything: the desktop has to be told the task ID before the request is known
// to have been delivered, and a request that never arrives must not become a
// task. Commit records it once delivery succeeded.
//
// The decision mirrors the desktop's own: its command watcher treats any
// request that arrives while the agent waits on a human as the reply.
func (s *Store) Plan(workerID, text string) (Route, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var route Route

	err := s.db.QueryRow(
		`SELECT i.id, e.id, e.task_id FROM interventions i
		 JOIN executions e ON e.id = i.execution_id
		 WHERE e.worker_id = ? AND e.status = ? AND i.replied_ns = 0
		 ORDER BY i.requested_ns DESC LIMIT 1`,
		workerID, StatusWaiting,
	).Scan(&route.InterventionID, &route.ExecutionID, &route.TaskID)

	switch {
	case err == nil:
		route.Kind = RouteReply
		return route, nil
	case err != sql.ErrNoRows:
		return Route{}, fmt.Errorf("tasks: routing request: %w", err)
	}

	var taskID, request, status string
	var updated int64

	err = s.db.QueryRow(
		`SELECT id, request, status, updated_ns FROM tasks WHERE worker_id = ? ORDER BY created_ns DESC LIMIT 1`,
		workerID,
	).Scan(&taskID, &request, &status, &updated)

	if err == nil && status == StatusError && normalize(request) == normalize(text) &&
		s.now().Sub(sqlitedb.FromNS(updated)) < retryWindow {
		return Route{Kind: RouteRetry, TaskID: taskID, ExecutionID: newID("x")}, nil
	}

	if err != nil && err != sql.ErrNoRows {
		return Route{}, fmt.Errorf("tasks: routing request: %w", err)
	}

	return Route{Kind: RouteNew, TaskID: newID("t"), ExecutionID: newID("x")}, nil
}

// Busy says why a worker cannot take on something new right now - one of its
// tasks is running, queued, or waiting on a human - or returns "" when it can.
//
// A replay is refused while any is. The desktop's watcher would take a replay
// sent to a paused agent as the reply to its pause, and one queued behind a
// running task would be swallowed as the reply if that task then paused
// (absorbQueuedReply) - either way the records would describe a replay that
// never ran.
func (s *Store) Busy(workerID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var status string

	err := s.db.QueryRow(
		`SELECT status FROM executions WHERE worker_id = ? AND status IN (?, ?, ?)
		 ORDER BY CASE status WHEN ? THEN 0 ELSE 1 END, started_ns DESC LIMIT 1`,
		workerID, StatusWaiting, StatusRunning, StatusQueued, StatusWaiting,
	).Scan(&status)

	switch {
	case err == sql.ErrNoRows:
		return "", nil
	case err != nil:
		return "", fmt.Errorf("tasks: checking worker: %w", err)
	case status == StatusWaiting:
		return "the agent is waiting for your reply to a task - answer it in the chat first", nil
	default:
		return "a task is still running on this desktop - wait for it to finish", nil
	}
}

// NewRoute is the route for a request whose planning failed: a new task. A
// request is still delivered when its record cannot be.
func NewRoute() Route {
	return Route{Kind: RouteNew, TaskID: newID("t"), ExecutionID: newID("x")}
}

// Commit records a delivered human request along the route Plan chose, and
// returns the route actually taken - which differs when the situation moved in
// between (the intervention it replied to was closed, say).
func (s *Store) Commit(route Route, worker Worker, text string) (Route, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.UpsertWorker(worker); err != nil {
		return route, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return route, fmt.Errorf("tasks: recording request: %w", err)
	}
	defer tx.Rollback()

	now := s.now()

	switch route.Kind {
	case RouteReply:
		var status string

		err := tx.QueryRow(`SELECT status FROM executions WHERE id = ?`, route.ExecutionID).Scan(&status)
		if err != nil || status != StatusWaiting && status != StatusRunning {
			// The pause it answered is over (its desktop was stopped): the
			// agent will take this as a new task, so record it as one.
			route = Route{Kind: RouteNew, TaskID: newID("t"), ExecutionID: newID("x")}
			if err := s.insertTask(tx, route, worker, text, now); err != nil {
				return route, err
			}
			break
		}

		// A second reply landing before the agent resumed joins the first.
		if _, err := tx.Exec(
			`UPDATE interventions SET
				reply = CASE WHEN reply = '' THEN ? ELSE reply || char(10) || char(10) || ? END,
				replied_ns = CASE WHEN replied_ns = 0 THEN ? ELSE replied_ns END
			 WHERE id = ?`,
			text, text, sqlitedb.ToNS(now), route.InterventionID,
		); err != nil {
			return route, fmt.Errorf("tasks: recording reply: %w", err)
		}

		if err := setExecutionStatus(tx, route.ExecutionID, route.TaskID, StatusRunning, now); err != nil {
			return route, err
		}

	case RouteRetry:
		var attempt int

		if err := tx.QueryRow(
			`SELECT COALESCE(MAX(attempt), 0) FROM executions WHERE task_id = ?`, route.TaskID,
		).Scan(&attempt); err != nil {
			return route, fmt.Errorf("tasks: recording retry: %w", err)
		}

		if err := s.insertExecution(tx, route, attempt+1, worker, now); err != nil {
			return route, err
		}

	default:
		route.Kind = RouteNew
		if err := s.insertTask(tx, route, worker, text, now); err != nil {
			return route, err
		}
	}

	if err := tx.Commit(); err != nil {
		return route, fmt.Errorf("tasks: recording request: %w", err)
	}

	return route, nil
}

func (s *Store) insertTask(tx *sql.Tx, route Route, worker Worker, text string, now time.Time) error {
	if _, err := tx.Exec(
		`INSERT INTO tasks (id, worker_id, engine, request, task_type, status, created_ns, updated_ns)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		route.TaskID, worker.ID, worker.Engine, text, Classify(text), StatusRunning,
		sqlitedb.ToNS(now), sqlitedb.ToNS(now),
	); err != nil {
		return fmt.Errorf("tasks: recording task: %w", err)
	}

	return s.insertExecution(tx, route, 1, worker, now)
}

// insertExecution starts an attempt. It is queued, not running, when the
// worker is already busy: its desktop runs one task at a time and holds the
// request until the current one ends.
func (s *Store) insertExecution(tx *sql.Tx, route Route, attempt int, worker Worker, now time.Time) error {
	status := StatusRunning

	var busy int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM executions WHERE worker_id = ? AND status IN (?, ?, ?)`,
		worker.ID, StatusRunning, StatusWaiting, StatusQueued,
	).Scan(&busy); err != nil {
		return fmt.Errorf("tasks: recording execution: %w", err)
	}

	if busy > 0 {
		status = StatusQueued
	}

	if _, err := tx.Exec(
		`INSERT INTO executions (id, task_id, attempt, worker_id, engine, model, status, started_ns)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		route.ExecutionID, route.TaskID, attempt, worker.ID, worker.Engine, worker.Model, status, sqlitedb.ToNS(now),
	); err != nil {
		return fmt.Errorf("tasks: recording execution: %w", err)
	}

	_, err := tx.Exec(`UPDATE tasks SET status = ?, updated_ns = ? WHERE id = ?`, status, sqlitedb.ToNS(now), route.TaskID)

	return err
}

// ---------------------------------------------------------------------------
// what a desktop reports
// ---------------------------------------------------------------------------

type executionRef struct {
	id      string
	taskID  string
	status  string
	endedAt time.Time
}

func (e executionRef) live() bool {
	return e.status == StatusQueued || e.status == StatusRunning || e.status == StatusWaiting
}

// resolve finds the execution a desktop's report belongs to: the one named by
// the task ID it echoed back, or - for a desktop built before task IDs, or a
// report that has none - the worker's oldest live execution, or the one that
// ended moments ago.
func resolve(tx *sql.Tx, workerID, taskID string, now time.Time) (executionRef, bool, error) {
	scan := func(row *sql.Row) (executionRef, bool, error) {
		var ref executionRef
		var ended int64

		err := row.Scan(&ref.id, &ref.taskID, &ref.status, &ended)
		if err == sql.ErrNoRows {
			return executionRef{}, false, nil
		}
		if err != nil {
			return executionRef{}, false, err
		}

		ref.endedAt = sqlitedb.FromNS(ended)

		return ref, true, nil
	}

	if taskID != "" {
		ref, ok, err := scan(tx.QueryRow(
			`SELECT id, task_id, status, ended_ns FROM executions
			 WHERE task_id = ? AND worker_id = ? ORDER BY attempt DESC LIMIT 1`,
			taskID, workerID,
		))
		if err != nil {
			return executionRef{}, false, err
		}

		// A task that ended long ago is not what a desktop is working on now,
		// whatever ID it last wrote down; fall through to what is live.
		if ok && (ref.live() || now.Sub(ref.endedAt) < lateWindow) {
			return ref, true, nil
		}
	}

	ref, ok, err := scan(tx.QueryRow(
		`SELECT id, task_id, status, ended_ns FROM executions
		 WHERE worker_id = ? AND status IN (?, ?)
		 ORDER BY started_ns LIMIT 1`,
		workerID, StatusRunning, StatusWaiting,
	))
	if err != nil || ok {
		return ref, ok, err
	}

	ref, ok, err = scan(tx.QueryRow(
		`SELECT id, task_id, status, ended_ns FROM executions
		 WHERE worker_id = ? AND status = ?
		 ORDER BY started_ns LIMIT 1`,
		workerID, StatusQueued,
	))
	if err != nil || ok {
		return ref, ok, err
	}

	ref, ok, err = scan(tx.QueryRow(
		`SELECT id, task_id, status, ended_ns FROM executions
		 WHERE worker_id = ? AND ended_ns >= ?
		 ORDER BY ended_ns DESC LIMIT 1`,
		workerID, sqlitedb.ToNS(now.Add(-lateWindow)),
	))

	return ref, ok, err
}

// promote marks a queued execution as the one running, from now.
func promote(tx *sql.Tx, ref *executionRef, now time.Time) error {
	if ref.status != StatusQueued {
		return nil
	}

	if _, err := tx.Exec(
		`UPDATE executions SET status = ?, started_ns = ? WHERE id = ?`,
		StatusRunning, sqlitedb.ToNS(now), ref.id,
	); err != nil {
		return err
	}

	ref.status = StatusRunning

	_, err := tx.Exec(`UPDATE tasks SET status = ?, updated_ns = ? WHERE id = ?`, StatusRunning, sqlitedb.ToNS(now), ref.taskID)

	return err
}

// AddStep records one step a desktop reported. It returns false when the step
// belongs to no task - work a desktop did with nobody having asked, such as a
// `gui` command typed by hand - which the records leave out.
func (s *Store) AddStep(workerID, taskID string, step Step) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("tasks: recording step: %w", err)
	}
	defer tx.Rollback()

	now := s.now()

	ref, ok, err := resolve(tx, workerID, taskID, now)
	if err != nil {
		return false, fmt.Errorf("tasks: recording step: %w", err)
	}

	if !ok {
		return false, nil
	}

	if err := promote(tx, &ref, now); err != nil {
		return false, fmt.Errorf("tasks: recording step: %w", err)
	}

	action := ""

	if step.Action != nil {
		encoded, err := json.Marshal(step.Action)
		if err != nil {
			return false, err
		}
		action = string(encoded)
	}

	if step.ID == "" {
		step.ID = newID("s")
	}

	if step.CreatedAt.IsZero() {
		step.CreatedAt = now
	}

	if _, err := tx.Exec(
		`INSERT INTO execution_steps (id, execution_id, text, action, created_ns) VALUES (?, ?, ?, ?, ?)`,
		step.ID, ref.id, step.Text, action, sqlitedb.ToNS(step.CreatedAt),
	); err != nil {
		return false, fmt.Errorf("tasks: recording step: %w", err)
	}

	// The agent worked the screen, which makes this on-screen work whatever
	// the request's words suggested - except a payment or message (a real
	// email/SMS/WhatsApp send), whose approvals are the point of that
	// bucket, and a person's recorded demonstration, whose every step is on
	// screen.
	if step.Action != nil {
		if _, err := tx.Exec(
			`UPDATE tasks SET task_type = ? WHERE id = ? AND task_type NOT IN (?, ?)`,
			TypeScreen, ref.taskID, TypeConsequential, TypeDemonstration,
		); err != nil {
			return false, fmt.Errorf("tasks: recording step: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("tasks: recording step: %w", err)
	}

	return true, nil
}

// RecordOutcome records how a desktop's turn ended, and returns the ID of the
// task it belonged to ("" when it matched none).
func (s *Store) RecordOutcome(workerID, taskID string, outcome Outcome) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return "", fmt.Errorf("tasks: recording outcome: %w", err)
	}
	defer tx.Rollback()

	now := s.now()

	ref, ok, err := resolve(tx, workerID, taskID, now)
	if err != nil {
		return "", fmt.Errorf("tasks: recording outcome: %w", err)
	}

	// A closed execution only takes steps after the fact, never a second end.
	if !ok || !ref.live() {
		return "", nil
	}

	if err := promote(tx, &ref, now); err != nil {
		return "", fmt.Errorf("tasks: recording outcome: %w", err)
	}

	if _, err := tx.Exec(
		`UPDATE executions SET
			engine = CASE WHEN ? != '' THEN ? ELSE engine END,
			model = CASE WHEN ? != '' THEN ? ELSE model END,
			cost_usd = COALESCE(?, cost_usd),
			input_tokens = COALESCE(?, input_tokens),
			output_tokens = COALESCE(?, output_tokens)
		 WHERE id = ?`,
		outcome.Engine, outcome.Engine, outcome.Model, outcome.Model,
		nullFloat(outcome.CostUSD), nullInt(outcome.InputTokens), nullInt(outcome.OutputTokens),
		ref.id,
	); err != nil {
		return "", fmt.Errorf("tasks: recording outcome: %w", err)
	}

	switch outcome.Kind {
	case OutcomeQuery, OutcomeApproval:
		kind := InterventionQuery
		if outcome.Kind == OutcomeApproval {
			kind = InterventionApproval
		}

		if _, err := tx.Exec(
			`INSERT INTO interventions (id, execution_id, kind, reason, request, requested_ns) VALUES (?, ?, ?, ?, ?, ?)`,
			newID("i"), ref.id, kind, InterventionReason(kind, outcome.Reply), outcome.Reply, sqlitedb.ToNS(now),
		); err != nil {
			return "", fmt.Errorf("tasks: recording intervention: %w", err)
		}

		if err := setExecutionStatus(tx, ref.id, ref.taskID, StatusWaiting, now); err != nil {
			return "", err
		}

		if err := absorbQueuedReply(tx, workerID, ref, now); err != nil {
			return "", err
		}

	default:
		status := StatusDone
		if outcome.Kind == OutcomeError {
			status = StatusError
		}

		if _, err := tx.Exec(
			`UPDATE executions SET status = ?, final_reply = ?, ended_ns = ? WHERE id = ?`,
			status, outcome.Reply, sqlitedb.ToNS(now), ref.id,
		); err != nil {
			return "", fmt.Errorf("tasks: recording outcome: %w", err)
		}

		if _, err := tx.Exec(
			`UPDATE tasks SET status = ?, updated_ns = ? WHERE id = ?`, status, sqlitedb.ToNS(now), ref.taskID,
		); err != nil {
			return "", fmt.Errorf("tasks: recording outcome: %w", err)
		}

		if err := promoteNext(tx, workerID, now); err != nil {
			return "", err
		}
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("tasks: recording outcome: %w", err)
	}

	return ref.taskID, nil
}

// promoteNext starts the worker's oldest queued execution, if it has one and
// nothing else is live: the desktop picks up its next held request as soon as
// the current task ends.
func promoteNext(tx *sql.Tx, workerID string, now time.Time) error {
	var live int

	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM executions WHERE worker_id = ? AND status IN (?, ?)`,
		workerID, StatusRunning, StatusWaiting,
	).Scan(&live); err != nil || live > 0 {
		return err
	}

	var ref executionRef

	err := tx.QueryRow(
		`SELECT id, task_id, status FROM executions WHERE worker_id = ? AND status = ? ORDER BY started_ns LIMIT 1`,
		workerID, StatusQueued,
	).Scan(&ref.id, &ref.taskID, &ref.status)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}

	return promote(tx, &ref, now)
}

// absorbQueuedReply mirrors what the desktop does with a request that was
// held behind a task which then stopped to ask a human: its command watcher
// hands that held request to the paused task as the reply. The records say
// the same - the queued "task" becomes the reply it really was.
func absorbQueuedReply(tx *sql.Tx, workerID string, paused executionRef, now time.Time) error {
	var queuedID, queuedTask, request string

	err := tx.QueryRow(
		`SELECT e.id, e.task_id, t.request FROM executions e JOIN tasks t ON t.id = e.task_id
		 WHERE e.worker_id = ? AND e.status = ? AND e.id != ?
		 ORDER BY e.started_ns LIMIT 1`,
		workerID, StatusQueued, paused.id,
	).Scan(&queuedID, &queuedTask, &request)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("tasks: matching held reply: %w", err)
	}

	if _, err := tx.Exec(
		`UPDATE interventions SET reply = ?, replied_ns = ?
		 WHERE id = (SELECT id FROM interventions WHERE execution_id = ? ORDER BY requested_ns DESC LIMIT 1)`,
		request, sqlitedb.ToNS(now), paused.id,
	); err != nil {
		return fmt.Errorf("tasks: matching held reply: %w", err)
	}

	if err := setExecutionStatus(tx, paused.id, paused.taskID, StatusRunning, now); err != nil {
		return err
	}

	if _, err := tx.Exec(`DELETE FROM executions WHERE id = ?`, queuedID); err != nil {
		return fmt.Errorf("tasks: matching held reply: %w", err)
	}

	_, err = tx.Exec(
		`DELETE FROM tasks WHERE id = ? AND NOT EXISTS (SELECT 1 FROM executions WHERE task_id = ?)`,
		queuedTask, queuedTask,
	)

	return err
}

func setExecutionStatus(tx *sql.Tx, executionID, taskID, status string, now time.Time) error {
	if _, err := tx.Exec(`UPDATE executions SET status = ? WHERE id = ?`, status, executionID); err != nil {
		return fmt.Errorf("tasks: updating execution: %w", err)
	}

	if _, err := tx.Exec(`UPDATE tasks SET status = ?, updated_ns = ? WHERE id = ?`, status, sqlitedb.ToNS(now), taskID); err != nil {
		return fmt.Errorf("tasks: updating task: %w", err)
	}

	return nil
}

// CloseOpen ends every live execution on a worker whose desktop is being
// stopped, restarted or deleted: its agent loses what it was doing, so a
// running task is abandoned and one waiting on a human ended waiting.
func (s *Store) CloseOpen(workerID, why string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := sqlitedb.ToNS(s.now())

	rows, err := tx.Query(
		`SELECT id, task_id, status FROM executions WHERE worker_id = ? AND status IN (?, ?, ?)`,
		workerID, StatusQueued, StatusRunning, StatusWaiting,
	)
	if err != nil {
		return err
	}

	var open []executionRef

	for rows.Next() {
		var ref executionRef
		if err := rows.Scan(&ref.id, &ref.taskID, &ref.status); err != nil {
			rows.Close()
			return err
		}
		open = append(open, ref)
	}
	rows.Close()

	for _, ref := range open {
		status := StatusAbandoned
		if ref.status == StatusWaiting {
			status = StatusQuery
		}

		if _, err := tx.Exec(
			`UPDATE executions SET status = ?, ended_ns = ?, final_reply = CASE WHEN final_reply = '' THEN ? ELSE final_reply END WHERE id = ?`,
			status, now, why, ref.id,
		); err != nil {
			return err
		}

		if _, err := tx.Exec(`UPDATE tasks SET status = ?, updated_ns = ? WHERE id = ?`, status, now, ref.taskID); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// SetVerdict records a human's verdict on a task: "correct", "wrong", or "" to
// withdraw one. The note is optional, and kept only with a verdict.
func (s *Store) SetVerdict(taskID, verdict, note string) (Task, error) {
	switch verdict {
	case VerdictCorrect, VerdictWrong, "":
	default:
		return Task{}, fmt.Errorf("tasks: unknown verdict %q", verdict)
	}

	verdictAt := int64(0)
	if verdict != "" {
		verdictAt = sqlitedb.ToNS(s.now())
	} else {
		note = ""
	}

	result, err := s.db.Exec(
		`UPDATE tasks SET verdict = ?, verdict_note = ?, verdict_ns = ? WHERE id = ?`,
		verdict, strings.TrimSpace(note), verdictAt, taskID,
	)
	if err != nil {
		return Task{}, fmt.Errorf("tasks: recording verdict: %w", err)
	}

	if changed, _ := result.RowsAffected(); changed == 0 {
		return Task{}, ErrNotFound
	}

	detail, err := s.Get(taskID)

	return detail.Task, err
}

// ---------------------------------------------------------------------------
// reading
// ---------------------------------------------------------------------------

const taskColumns = `t.id, t.worker_id, COALESCE(w.name, t.worker_id), t.engine, t.request, t.task_type,
	t.status, t.verdict, t.verdict_note, t.verdict_ns, t.created_ns, t.updated_ns`

func scanTask(scanner interface{ Scan(...any) error }) (Task, error) {
	var task Task
	var verdictAt, created, updated int64

	if err := scanner.Scan(&task.ID, &task.WorkerID, &task.WorkerName, &task.Engine, &task.Request, &task.Type,
		&task.Status, &task.Verdict, &task.VerdictNote, &verdictAt, &created, &updated); err != nil {
		return Task{}, err
	}

	task.VerdictAt = sqlitedb.FromNS(verdictAt)
	task.CreatedAt = sqlitedb.FromNS(created)
	task.UpdatedAt = sqlitedb.FromNS(updated)

	return task, nil
}

// Get returns one task with its worker, executions, steps and interventions.
func (s *Store) Get(taskID string) (Detail, error) {
	row := s.db.QueryRow(
		`SELECT `+taskColumns+` FROM tasks t LEFT JOIN workers w ON w.id = t.worker_id WHERE t.id = ?`, taskID,
	)

	task, err := scanTask(row)
	if err == sql.ErrNoRows {
		return Detail{}, ErrNotFound
	}
	if err != nil {
		return Detail{}, fmt.Errorf("tasks: reading task: %w", err)
	}

	detail := Detail{Task: task, Executions: []Execution{}}
	detail.Worker, _ = s.Worker(task.WorkerID)

	executions, err := s.loadExecutions(`WHERE e.task_id = ?`, taskID)
	if err != nil {
		return Detail{}, err
	}

	index := make(map[string]int, len(executions))
	for i := range executions {
		index[executions[i].ID] = i
	}

	steps, err := s.db.Query(
		`SELECT s.id, s.execution_id, s.text, s.action, s.created_ns FROM execution_steps s
		 JOIN executions e ON e.id = s.execution_id WHERE e.task_id = ? ORDER BY s.rowid`, taskID,
	)
	if err != nil {
		return Detail{}, fmt.Errorf("tasks: reading steps: %w", err)
	}

	for steps.Next() {
		var step Step
		var executionID, action string
		var created int64

		if err := steps.Scan(&step.ID, &executionID, &step.Text, &action, &created); err != nil {
			steps.Close()
			return Detail{}, fmt.Errorf("tasks: reading steps: %w", err)
		}

		step.CreatedAt = sqlitedb.FromNS(created)

		if action != "" {
			var decoded Action
			if json.Unmarshal([]byte(action), &decoded) == nil {
				step.Action = &decoded
			}
		}

		if i, ok := index[executionID]; ok {
			executions[i].Steps = append(executions[i].Steps, step)
		}
	}
	steps.Close()

	interventions, err := s.loadInterventions(`WHERE e.task_id = ?`, taskID)
	if err != nil {
		return Detail{}, err
	}

	for _, intervention := range interventions {
		if i, ok := index[intervention.ExecutionID]; ok {
			executions[i].Interventions = append(executions[i].Interventions, intervention)
		}
	}

	if executions != nil {
		detail.Executions = executions
	}

	return detail, nil
}

func (s *Store) loadExecutions(where string, args ...any) ([]Execution, error) {
	rows, err := s.db.Query(
		`SELECT e.id, e.task_id, e.attempt, e.worker_id, e.engine, e.model, e.status, e.final_reply,
			e.started_ns, e.ended_ns, e.cost_usd, e.input_tokens, e.output_tokens
		 FROM executions e JOIN tasks t ON t.id = e.task_id `+where+` ORDER BY e.task_id, e.attempt`, args...,
	)
	if err != nil {
		return nil, fmt.Errorf("tasks: reading executions: %w", err)
	}
	defer rows.Close()

	var executions []Execution

	for rows.Next() {
		var execution Execution
		var started, ended int64
		var cost sql.NullFloat64
		var input, output sql.NullInt64

		if err := rows.Scan(&execution.ID, &execution.TaskID, &execution.Attempt, &execution.WorkerID,
			&execution.Engine, &execution.Model, &execution.Status, &execution.FinalReply,
			&started, &ended, &cost, &input, &output); err != nil {
			return nil, fmt.Errorf("tasks: reading executions: %w", err)
		}

		execution.StartedAt = sqlitedb.FromNS(started)
		execution.EndedAt = sqlitedb.FromNS(ended)
		execution.Steps = []Step{}
		execution.Interventions = []Intervention{}

		if cost.Valid {
			execution.CostUSD = &cost.Float64
		}
		if input.Valid {
			execution.InputTokens = &input.Int64
		}
		if output.Valid {
			execution.OutputTokens = &output.Int64
		}

		executions = append(executions, execution)
	}

	return executions, rows.Err()
}

func (s *Store) loadInterventions(where string, args ...any) ([]Intervention, error) {
	rows, err := s.db.Query(
		`SELECT i.id, i.execution_id, i.kind, i.reason, i.request, i.reply, i.requested_ns, i.replied_ns
		 FROM interventions i JOIN executions e ON e.id = i.execution_id JOIN tasks t ON t.id = e.task_id `+
			where+` ORDER BY i.requested_ns`, args...,
	)
	if err != nil {
		return nil, fmt.Errorf("tasks: reading interventions: %w", err)
	}
	defer rows.Close()

	var interventions []Intervention

	for rows.Next() {
		var intervention Intervention
		var requested, replied int64

		if err := rows.Scan(&intervention.ID, &intervention.ExecutionID, &intervention.Kind, &intervention.Reason,
			&intervention.Request, &intervention.Reply, &requested, &replied); err != nil {
			return nil, fmt.Errorf("tasks: reading interventions: %w", err)
		}

		intervention.RequestedAt = sqlitedb.FromNS(requested)
		intervention.RepliedAt = sqlitedb.FromNS(replied)

		interventions = append(interventions, intervention)
	}

	return interventions, rows.Err()
}

func normalize(text string) string {
	return strings.Join(strings.Fields(strings.ToLower(text)), " ")
}

func nullFloat(value *float64) any {
	if value == nil {
		return nil
	}

	return *value
}

func nullInt(value *int64) any {
	if value == nil {
		return nil
	}

	return *value
}
