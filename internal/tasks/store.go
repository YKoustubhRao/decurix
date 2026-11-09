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
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
	"github.com/YKoustubhRao/decurix/internal/withhold"
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
	// Checked is what the latest attempt's final check said (nil: it said nothing), and Outcome what a person is
	// shown from Status and Checked together (DisplayOutcome): a "done" nobody checked is never shown as Done.
	Checked *bool   `json:"checked"`
	Outcome Display `json:"outcome"`
}

// Execution is one attempt at a task by one engine.
type Execution struct {
	ID           string    `json:"id"`
	TaskID       string    `json:"task_id"`
	Attempt      int       `json:"attempt"`
	WorkerID     string    `json:"worker_id"`
	Engine       string    `json:"engine"`
	Model        string    `json:"model"`
	Status       string    `json:"status"`
	FinalReply   string    `json:"final_reply"`
	StartedAt    time.Time `json:"started_at"`
	EndedAt      time.Time `json:"ended_at"`
	CostUSD      *float64  `json:"cost_usd"`
	InputTokens  *int64    `json:"input_tokens"`
	OutputTokens *int64    `json:"output_tokens"`
	// CachedInputTokens is the part of InputTokens served from the provider's
	// prompt cache; nil when the desktop did not say.
	CachedInputTokens *int64 `json:"cached_input_tokens"`
	// Checked says whether the desktop's final check judged the result
	// before it was reported; nil when the desktop did not say.
	Checked *bool `json:"checked"`
	// Outcome is what a person is shown from Status and Checked (DisplayOutcome).
	Outcome Display `json:"outcome"`
	// Verdict is the final check as the desktop reported it (decurix.verdict/v1); nil for an older image's, which
	// said only Checked. Checked is derived from it.
	Verdict *GateVerdict `json:"verdict"`
	// Origin is what started this execution (unknown before R7); Skill the taught skill and version that drove it
	// (empty: none said); ResolvedModel the deployment that served its model calls (R7).
	Origin        Origin         `json:"origin"`
	Skill         Skill          `json:"skill"`
	ResolvedModel string         `json:"resolved_model"`
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
	// Secret says the desktop asked this for a secret (a password or code field): its reply is kept withheld whole.
	Secret bool `json:"secret"`
	// ReplierID is who answered it, and JudgementID the judgement that answer is ("" before plan R5).
	ReplierID   string `json:"replier_id"`
	JudgementID string `json:"judgement_id"`
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
	// CachedInputTokens: the part of InputTokens read from the prompt cache.
	CachedInputTokens *int64
	// Checked: whether the final check judged a finished result (nil: not said).
	Checked *bool
	// Verdict: the final check, structured (nil: an older image). When given, the stored checked flag is derived
	// from it (done), and is false whenever Checked says otherwise.
	Verdict *GateVerdict
	// Skill: the taught skill that drove the turn, and its version (nil: none said).
	Skill *Skill
	// Secret: a query raised for a secret (the agent knows where its question came from); the person's reply to it
	// is kept as withhold.Secret in every record, and reaches the desktop as typed.
	Secret bool
	// Interrupted: the desktop restarted under the task and its agent reports it, after the fact - the task stopped
	// part way, which is no failure of the work: it is recorded as stopped (StatusAbandoned), never as failed.
	Interrupted bool
}

// Route is where a human request goes: a new task, the reply to a task's open
// intervention, or another attempt at a task that just failed.
type Route struct {
	Kind           string `json:"kind"`
	TaskID         string `json:"task_id"`
	ExecutionID    string `json:"execution_id"`
	InterventionID string `json:"intervention_id,omitempty"`
	// JudgementID is the judgement a reply is: chosen before delivery (NewJudgementID), so the desktop is told it
	// with the reply, and after CommitReply the one actually made ("" for none).
	JudgementID string `json:"judgement_id,omitempty"`
	// Origin is what started the execution this route opens or continues (C12); written on a new execution.
	Origin Origin `json:"origin"`
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
	// Whether the desktop's final check (verify.py, the skill gate) judged
	// the result: 1 checked, 0 not, NULL not said (an older desktop image).
	`ALTER TABLE executions ADD COLUMN checked INTEGER`,
	// The part of input_tokens the provider read from its prompt cache (billed
	// at a fraction): what a task really cost, beside the raw count. NULL when
	// the desktop did not say.
	`ALTER TABLE executions ADD COLUMN cached_input_tokens INTEGER`,
	// Whether the desktop asked this question for a secret (review 2026-11-03, X-6): its reply is withheld whole.
	`ALTER TABLE interventions ADD COLUMN secret INTEGER NOT NULL DEFAULT 0`,
	// Every human input with its author (plan R5), append-only: answers, approvals, verdicts, and what a desktop kept
	// from a person's reply (citing it in derived_from). Written by insertJudgement only; never updated or deleted,
	// and kept when its worker is deleted (judgements.go).
	`CREATE TABLE judgements (
		id TEXT PRIMARY KEY, at_ns INTEGER NOT NULL,
		actor_id TEXT NOT NULL, actor_name TEXT NOT NULL DEFAULT '', actor_roles TEXT NOT NULL DEFAULT '[]',
		actor_kind TEXT NOT NULL,
		door TEXT NOT NULL DEFAULT '',
		kind TEXT NOT NULL,
		worker_id TEXT NOT NULL DEFAULT '', task_id TEXT NOT NULL DEFAULT '', execution_id TEXT NOT NULL DEFAULT '',
		intervention_id TEXT NOT NULL DEFAULT '', skill_name TEXT NOT NULL DEFAULT '', skill_version INTEGER NOT NULL DEFAULT 0,
		text TEXT NOT NULL DEFAULT '',
		secret INTEGER NOT NULL DEFAULT 0,
		payload TEXT NOT NULL DEFAULT '{}',
		changed TEXT NOT NULL DEFAULT '{}',
		derived_from TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX judgements_task ON judgements (task_id, at_ns)`,
	`CREATE INDEX judgements_worker ON judgements (worker_id, at_ns)`,
	// Who answered an intervention, and the judgement that answer is.
	`ALTER TABLE interventions ADD COLUMN replier_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE interventions ADD COLUMN judgement_id TEXT NOT NULL DEFAULT ''`,
	// The run's final check, structured (plan R6, decurix.verdict/v1): '' for an older image, which sent only checked.
	`ALTER TABLE executions ADD COLUMN verdict TEXT NOT NULL DEFAULT ''`,
	// What each execution ran (plan R7): what started it (a person, a schedule, a DAG node, ...), the taught skill
	// and its version that drove it, and the model deployment that served it. '' / 0 before R7, read as unknown.
	`ALTER TABLE executions ADD COLUMN origin TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE executions ADD COLUMN origin_ref TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE executions ADD COLUMN origin_actor TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE executions ADD COLUMN skill_name TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE executions ADD COLUMN skill_version INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE executions ADD COLUMN resolved_model TEXT NOT NULL DEFAULT ''`,
}

// Store is the SQLite-backed record of every worker's tasks.
type Store struct {
	db *sql.DB

	// mu serialises the store's read-decide-write sequences (routing a
	// request, closing one execution and promoting the next), which each span
	// several statements.
	mu sync.Mutex

	now func() time.Time

	// onEnded is told of every task that closed (OnTaskEnded), after the close is committed and the store unlocked.
	onEnded atomic.Pointer[func(workerID, taskID string)]
}

// OnTaskEnded sets f to be called with the worker and the task each time a task closes: done, error, or ended by
// CloseOpen (stopped, restarted, deleted, abandoned). f runs after the close is committed, outside the store's lock,
// on the closing caller's goroutine, so it must not block; nothing it does can undo or fail the close.
func (s *Store) OnTaskEnded(f func(workerID, taskID string)) {
	s.onEnded.Store(&f)
}

// tellEnded calls the OnTaskEnded hook, if any, for each task.
func (s *Store) tellEnded(workerID string, taskIDs ...string) {
	f := s.onEnded.Load()
	if f == nil || *f == nil {
		return
	}
	for _, id := range taskIDs {
		if id != "" {
			(*f)(workerID, id)
		}
	}
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
	return sqlitedb.NewID(prefix, 8)
}

// ValidID reports whether id has the shape of a task ID this store issues
// ("t" and 16 hex digits), which is how a value reported back by a desktop is
// checked before it is used - it travels into a file name (C-23: the prefix
// was not checked).
func ValidID(id string) bool {
	if len(id) != 17 || id[0] != 't' {
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
	// Compared with what Commit stored, so withheld the same way (withhold.Text).
	text = withhold.Text(text)
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

	return NewRoute(), nil
}

// Busy says why a worker cannot take on something new right now - one of its
// tasks is running, queued, or waiting on a human - or returns "" when it can.
//
// A replay is refused while any is. The desktop's watcher would take a replay
// sent to a paused agent as the reply to its pause, and one queued behind a
// running task cannot run until that task finishes. Refuse it so the person
// gets a concrete busy result instead of an unstarted replay.
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

// OpenRequest returns the request of the task a worker has running or waiting on a person, or "" when it has none -
// what a check of that task's result may refer to.
func (s *Store) OpenRequest(workerID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var request string
	err := s.db.QueryRow(
		`SELECT t.request FROM executions e JOIN tasks t ON t.id = e.task_id
		 WHERE e.worker_id = ? AND e.status IN (?, ?) ORDER BY e.started_ns DESC LIMIT 1`,
		workerID, StatusRunning, StatusWaiting,
	).Scan(&request)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("tasks: reading the open request: %w", err)
	}
	return request, nil
}

// Working reports whether any desktop has a task running or queued - one of
// the things that should keep this machine from going to sleep (see
// internal/keepawake): a sleep in the middle of a task freezes it half done.
// A task waiting on a person is not counted; that can wait for days.
func (s *Store) Working() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM executions WHERE status IN (?, ?)`, StatusRunning, StatusQueued,
	).Scan(&n); err != nil {
		return false
	}

	return n > 0
}

// OpenQuestion returns the question a task is waiting on a person for - its live execution's newest unanswered
// intervention - and whether the desktop asked it for a secret. ok is false when the task waits on nothing. It is how
// another door (the concierge) keeps a reply to it the way Commit does.
func (s *Store) OpenQuestion(taskID string) (question string, secret, ok bool) {
	err := s.db.QueryRow(
		`SELECT i.request, i.secret FROM interventions i JOIN executions e ON e.id = i.execution_id
		 WHERE e.task_id = ? AND e.status = ? AND i.replied_ns = 0 ORDER BY i.requested_ns DESC LIMIT 1`,
		taskID, StatusWaiting,
	).Scan(&question, &secret)
	if err != nil {
		return "", false, false
	}
	return question, secret, true
}

// LiveWorkers returns every worker that has a live execution - queued, running or waiting on a human - so the
// control plane can close those whose desktop is gone (internal/idlepause, C-17).
func (s *Store) LiveWorkers() ([]string, error) {
	workers, err := sqlitedb.QueryAll(s.db, func(r sqlitedb.Scanner) (string, error) {
		var id string
		err := r.Scan(&id)
		return id, err
	}, `SELECT DISTINCT worker_id FROM executions WHERE status IN (?, ?, ?) ORDER BY worker_id`,
		StatusQueued, StatusRunning, StatusWaiting)
	if err != nil {
		return nil, fmt.Errorf("tasks: listing workers with live tasks: %w", err)
	}
	return workers, nil
}

// IsOpen reports whether a task still has a live execution on a worker -
// queued, running or waiting on a human. A desktop that restarted on its own
// reports the task it lost; one restarted through the API had that task
// closed already (CloseOpen), and its report must not be recorded twice.
func (s *Store) IsOpen(workerID, taskID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var one int
	err := s.db.QueryRow(
		`SELECT 1 FROM executions WHERE worker_id = ? AND task_id = ? AND status IN (?, ?, ?) LIMIT 1`,
		workerID, taskID, StatusQueued, StatusRunning, StatusWaiting,
	).Scan(&one)

	switch {
	case err == sql.ErrNoRows:
		return false, nil
	case err != nil:
		return false, fmt.Errorf("tasks: checking task: %w", err)
	}
	return true, nil
}

// Waiting returns the tasks whose execution on a worker waits on a human (a question or an approval it asked), oldest
// first. A desktop that comes back from a restart is asked whether it still holds each: a question its agent no
// longer has is closed by the agent's own report (found live 2026-11-09: a task read "waiting on you" for good over a
// question the restarted agent had forgotten).
func (s *Store) Waiting(workerID string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ids, err := sqlitedb.QueryAll(s.db, func(r sqlitedb.Scanner) (string, error) {
		var id string
		err := r.Scan(&id)
		return id, err
	}, `SELECT task_id FROM executions WHERE worker_id = ? AND status = ? ORDER BY started_ns`, workerID, StatusWaiting)
	if err != nil {
		return nil, fmt.Errorf("tasks: listing waiting tasks: %w", err)
	}
	return ids, nil
}

// LastActivity returns when a worker's task records were last touched - a
// task created, started, finished, or reopened by a human's reply - or
// ok=false when the worker has no task records at all yet. Used by
// internal/idlepause as the idle clock for a worker with nothing currently
// running: how long ago its last task last changed.
func (s *Store) LastActivity(workerID string) (t time.Time, ok bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var ns sql.NullInt64

	if err := s.db.QueryRow(
		`SELECT MAX(updated_ns) FROM tasks WHERE worker_id = ?`, workerID,
	).Scan(&ns); err != nil {
		return time.Time{}, false, fmt.Errorf("tasks: last activity: %w", err)
	}

	if !ns.Valid {
		return time.Time{}, false, nil
	}

	return sqlitedb.FromNS(ns.Int64), true, nil
}

// NewRoute is the route for a request whose planning failed: a new task. A
// request is still delivered when its record cannot be.
func NewRoute() Route {
	return Route{Kind: RouteNew, TaskID: newID("t"), ExecutionID: newID("x")}
}

// Commit records a delivered human request along the route Plan chose, and
// returns the route actually taken - which differs when the situation moved in
// between (the intervention it replied to was closed, say).
//
// origin is what started the request: a required argument, so no producer can open an execution without saying
// (plan R7); one stored unknown is logged.
func (s *Store) Commit(route Route, worker Worker, text string, origin Origin) (Route, error) {
	route, _, err := s.CommitReply(route, worker, text, false, ReplyBy{}, origin)
	return route, err
}

// ReplyBy is who sent a human request and through which door: a reply's judgement is made from it. Approval is the
// access log's decision on an approval reply ({"approval_id": ..., "outcome": ...}), merged into its payload.
type ReplyBy struct {
	Actor    Actor
	Door     string
	Approval map[string]any
}

// CommitReply is Commit that also returns the text it stored - what the chat, the send response and every other copy
// must keep, so there is one rule for what a person typed (review 2026-11-03, C-05). secret is the person's own mark
// ("this is a secret"): the whole text is kept as withhold.Secret, as is a reply to a question the desktop asked for
// a secret. Otherwise the text is withheld by withhold.Text, and a reply by withhold.Reply against its question.
//
// A reply to an open intervention is also a judgement (kind answer, or approval when it answers an approval) by
// by.Actor, written in the same transaction under route.JudgementID when the caller chose one (it went to the desktop
// with the reply); the intervention keeps the replier and the judgement id. The route returned carries the
// judgement's id, "" when none was made (a new task, or a reply with no actor, which no door sends).
func (s *Store) CommitReply(route Route, worker Worker, text string, secret bool, by ReplyBy, origin Origin) (Route, string, error) {
	route.Origin = origin
	// A secret the person typed (a password, an OTP) reaches the desktop as written, never these records: they are
	// shown in traces, reports and exports (review 2026-11-02).
	stored := requestForm(text, secret)
	route, err := s.commit(route, worker, &stored, by, secret, text)
	return route, stored, err
}

// requestForm is how the records keep a request as sent: whole as withhold.Secret when the person marked it secret,
// otherwise with what withhold.Text finds withheld.
func requestForm(text string, marked bool) string {
	if marked {
		return withhold.Secret
	}
	return withhold.Text(text)
}

// replyForm is how the records keep a reply already in its request form (requestForm), against the question it
// answers: whole as withhold.Secret when the question was asked for a secret, otherwise with the bare values a
// question for a secret asks for withheld too (withhold.Reply).
func replyForm(stored, question string, askedSecret bool) string {
	if askedSecret {
		return withhold.Secret
	}
	return withhold.Reply(question, stored)
}

// RequestForm is the C14 decision on a new request where no route is kept (a practice or eval case): how the records
// keep it, as StoredForm decides it for a request, and whether anything of it is withheld.
func RequestForm(text string, marked bool) (string, bool) {
	stored := requestForm(text, marked)
	return stored, stored != text
}

// ReplyForm is the C14 decision on a reply to question where no route is kept (an eval expert's reply): as
// StoredForm decides it for a reply against the question open now, and whether anything of it is withheld.
func ReplyForm(text string, marked bool, question string, askedSecret bool) (string, bool) {
	stored := replyForm(requestForm(text, marked), question, askedSecret)
	return stored, stored != text
}

// StoredForm is how the records will keep text sent along route, decided before it is sent by the same rules
// CommitReply keeps it by (requestForm, then replyForm against the question open now), and whether anything of it
// is withheld. The control plane tells the desktop "secret" exactly when this says withheld (contract C14), so the
// two sides never disagree on what is a secret.
func (s *Store) StoredForm(route Route, text string, marked bool) (string, bool) {
	stored := requestForm(text, marked)
	if route.Kind == RouteReply {
		var question string
		var askedSecret bool
		err := s.db.QueryRow(`SELECT request, secret FROM interventions WHERE execution_id = ? AND replied_ns = 0
			ORDER BY requested_ns DESC LIMIT 1`, route.ExecutionID).Scan(&question, &askedSecret)
		if err != nil && route.InterventionID != "" {
			err = s.db.QueryRow(`SELECT request, secret FROM interventions WHERE id = ?`, route.InterventionID).
				Scan(&question, &askedSecret)
		}
		if err == nil {
			stored = replyForm(stored, question, askedSecret)
		}
	}
	return stored, stored != text
}

// commit is Commit's body. It may withhold *stored further (a reply to a question asked for a secret), and leaves in
// it what it recorded.
func (s *Store) commit(route Route, worker Worker, stored *string, by ReplyBy, secret bool, raw string) (Route, error) {
	judgementID := route.JudgementID
	route.JudgementID = ""
	text := *stored
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
			route = NewRoute()
			if err := s.insertTask(tx, route, worker, text, now); err != nil {
				return route, err
			}
			break
		}

		interventionID := route.InterventionID

		if status == StatusWaiting {
			// The execution can have resumed on a faster reply and paused
			// again on a *new* intervention since Plan picked this route -
			// always attach to whichever intervention is actually open now,
			// never the one Plan resolved, so a delayed reply can't land on
			// an intervention that has already been answered and closed
			// while this one's delivery was still in flight.
			var currentID string
			if err := tx.QueryRow(
				`SELECT id FROM interventions WHERE execution_id = ? AND replied_ns = 0
				 ORDER BY requested_ns DESC LIMIT 1`,
				route.ExecutionID,
			).Scan(&currentID); err == nil {
				interventionID = currentID
				route.InterventionID = currentID
			}
		}

		// An answer to a question the desktop asked for a secret is kept withheld whole; one whose words ask for a
		// secret (an OTP, a password, a PIN) is kept withheld even when it is a bare value (withhold.Reply). text is
		// already withheld as any request is.
		var question, interventionKind string
		var askedSecret bool
		if err := tx.QueryRow(`SELECT request, secret, kind FROM interventions WHERE id = ?`, interventionID).Scan(&question, &askedSecret, &interventionKind); err == nil {
			*stored = replyForm(*stored, question, askedSecret)
			text = *stored
		}

		// The reply is a judgement by whoever sent it (plan R5). A reply with no actor is still recorded (it has
		// been delivered), without one: every door that sends a reply carries its actor (httpapi refuses otherwise).
		if by.Actor.ID != "" && interventionID != "" {
			kind, payload := JudgementAnswer, map[string]any{}
			if interventionKind == InterventionApproval {
				kind = JudgementApproval
				for k, v := range by.Approval {
					payload[k] = v
				}
			}
			var workerID string
			_ = tx.QueryRow(`SELECT worker_id FROM executions WHERE id = ?`, route.ExecutionID).Scan(&workerID)
			// Secret is the one decision C14 tells the desktop by: the records withhold any of it (StoredForm).
			payloadJSON, _ := json.Marshal(payload)
			id, err := insertJudgement(tx, Judgement{ID: judgementID, At: now, Actor: by.Actor, Door: by.Door,
				Kind: kind, WorkerID: workerID, TaskID: route.TaskID, ExecutionID: route.ExecutionID,
				InterventionID: interventionID, Text: text, Secret: secret || askedSecret || text != raw, Payload: payloadJSON})
			if err != nil {
				return route, err
			}
			if _, err := tx.Exec(`UPDATE interventions SET replier_id = CASE WHEN replier_id = '' THEN ? ELSE replier_id END,
				judgement_id = CASE WHEN judgement_id = '' THEN ? ELSE judgement_id END WHERE id = ?`,
				by.Actor.ID, id, interventionID); err != nil {
				return route, fmt.Errorf("tasks: recording who replied: %w", err)
			}
			route.JudgementID = id
		}

		// A second reply landing before the agent resumed joins the first.
		if _, err := tx.Exec(
			`UPDATE interventions SET
				reply = CASE WHEN reply = '' THEN ? ELSE reply || char(10) || char(10) || ? END,
				replied_ns = CASE WHEN replied_ns = 0 THEN ? ELSE replied_ns END
			 WHERE id = ?`,
			text, text, sqlitedb.ToNS(now), interventionID,
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
	// Every producer names what started its work (plan R7): an origin with no kind is refused, loudly, and one named
	// unknown on purpose is stored and logged.
	if route.Origin.Kind == "" {
		return fmt.Errorf("%w: execution %s of %s on %s", ErrNoOrigin, route.ExecutionID, route.TaskID, worker.ID)
	}
	if originKind(route.Origin) == OriginUnknown {
		log.Printf("tasks: execution %s of %s on %s has no known origin (%q): stored as unknown", route.ExecutionID, route.TaskID, worker.ID, route.Origin.Kind)
	}
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
		`INSERT INTO executions (id, task_id, attempt, worker_id, engine, model, status, started_ns, origin, origin_ref,
			origin_actor) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		route.ExecutionID, route.TaskID, attempt, worker.ID, worker.Engine, worker.Model, status, sqlitedb.ToNS(now),
		originKind(route.Origin), route.Origin.Ref, route.Origin.ActorID,
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

// Live reports whether an execution or task status is still open: queued,
// running or waiting on a human.
func Live(status string) bool {
	return status == StatusQueued || status == StatusRunning || status == StatusWaiting
}

// resolve finds the execution a desktop's report belongs to. A report that
// names a task belongs to that task's latest attempt on this worker, live or
// not - never to whatever else is live: a stale or resent outcome for a task
// that ended must not close another one (review 2026-11-03, C-01; the agent
// resends a kept outcome for a day). A named task this worker never had
// matches nothing. Only a report with no task ID (a desktop built before task
// IDs) falls back to the worker's oldest live execution, or the one that ended
// moments ago.
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
		if !ok {
			log.Printf("tasks: %s reported on task %s, which it never had: not recorded", workerID, taskID)
		}
		return ref, ok, nil
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
	// A value held for its owner's life (a desktop's screen password) is withheld as the desktop may have written it.
	step.Text = withhold.Known(step.Text)
	if step.Action != nil {
		a := *step.Action
		a.Target, a.Change, a.Reason = withhold.Known(a.Target), withhold.Known(a.Change), withhold.Known(a.Reason)
		step.Action = &a
	}
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

	// A step for a task that ended is that task's only in the moments after (its last line can trail the reply);
	// later it is work nobody asked for, such as a `gui` command typed by hand while the desktop still names its
	// last task. It is left out - never moved onto whatever task is live now.
	if !ok || !Live(ref.status) && now.Sub(ref.endedAt) >= lateWindow {
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
			`UPDATE tasks SET task_type = ? WHERE id = ? AND task_type NOT IN (?, ?, ?)`,
			TypeScreen, ref.taskID, TypeConsequential, TypeDemonstration, TypeTeaching,
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
// task it belonged to: "" when it matched none, or named a task that has
// already ended (a closed execution takes no second end).
func (s *Store) RecordOutcome(workerID, taskID string, outcome Outcome) (string, error) {
	task, _, err := s.RecordOutcomeExec(workerID, taskID, outcome)
	return task, err
}

// RecordOutcomeExec is RecordOutcome, also returning the execution the outcome closed or paused ("" with the task).
func (s *Store) RecordOutcomeExec(workerID, taskID string, outcome Outcome) (string, string, error) {
	outcome.Reply = withhold.Known(outcome.Reply) // as AddStep
	ended := ""
	defer func() { s.tellEnded(workerID, ended) }() // runs after the unlock below
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return "", "", fmt.Errorf("tasks: recording outcome: %w", err)
	}
	defer tx.Rollback()

	now := s.now()

	ref, ok, err := resolve(tx, workerID, taskID, now)
	if err != nil {
		return "", "", fmt.Errorf("tasks: recording outcome: %w", err)
	}

	// A closed execution only takes steps after the fact, never a second end.
	if !ok || !Live(ref.status) {
		return "", "", nil
	}

	if err := promote(tx, &ref, now); err != nil {
		return "", "", fmt.Errorf("tasks: recording outcome: %w", err)
	}

	// The structured verdict decides checked; a callback whose own flag disagrees, or that says checked with no
	// verdict at all, is stored not checked (fail closed).
	checked, disagree := checkedFrom(outcome.Verdict, outcome.Checked)
	if disagree {
		said := "none"
		if outcome.Verdict != nil {
			said = outcome.Verdict.Verdict
		}
		log.Printf("tasks: verdict and checked disagree for %s (verdict %s, checked %v): stored as not checked",
			ref.id, said, *outcome.Checked)
	}
	skillName, skillVersion := "", 0
	if outcome.Skill != nil && strings.TrimSpace(outcome.Skill.Name) != "" {
		skillName, skillVersion = clipText(strings.TrimSpace(outcome.Skill.Name), 128), max(outcome.Skill.Version, 0)
	}
	verdictJSON := ""
	if outcome.Verdict != nil {
		if b, err := json.Marshal(outcome.Verdict); err == nil {
			verdictJSON = string(b)
		}
	}

	if _, err := tx.Exec(
		`UPDATE executions SET
			engine = CASE WHEN ? != '' THEN ? ELSE engine END,
			model = CASE WHEN ? != '' THEN ? ELSE model END,
			cost_usd = COALESCE(?, cost_usd),
			input_tokens = COALESCE(?, input_tokens),
			output_tokens = COALESCE(?, output_tokens),
			cached_input_tokens = COALESCE(?, cached_input_tokens),
			checked = COALESCE(?, checked),
			verdict = CASE WHEN ? != '' THEN ? ELSE verdict END,
			skill_name = CASE WHEN ? != '' THEN ? ELSE skill_name END,
			skill_version = CASE WHEN ? != '' THEN ? ELSE skill_version END
		 WHERE id = ?`,
		outcome.Engine, outcome.Engine, outcome.Model, outcome.Model,
		nullable(outcome.CostUSD), nullable(outcome.InputTokens), nullable(outcome.OutputTokens),
		nullable(outcome.CachedInputTokens), nullBool(checked), verdictJSON, verdictJSON,
		skillName, skillName, skillName, skillVersion, ref.id,
	); err != nil {
		return "", "", fmt.Errorf("tasks: recording outcome: %w", err)
	}

	switch outcome.Kind {
	case OutcomeQuery, OutcomeApproval:
		kind := InterventionQuery
		if outcome.Kind == OutcomeApproval {
			kind = InterventionApproval
		}

		if _, err := tx.Exec(
			`INSERT INTO interventions (id, execution_id, kind, reason, request, requested_ns, secret) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			newID("i"), ref.id, kind, InterventionReason(kind, outcome.Reply), outcome.Reply, sqlitedb.ToNS(now), outcome.Secret,
		); err != nil {
			return "", "", fmt.Errorf("tasks: recording intervention: %w", err)
		}

		if err := setExecutionStatus(tx, ref.id, ref.taskID, StatusWaiting, now); err != nil {
			return "", "", err
		}

	default:
		status := StatusDone
		if outcome.Kind == OutcomeError {
			status = StatusError
		}
		if outcome.Interrupted {
			// Found live (2026-11-08): two bills saved correctly before a restart read "Failed" in their records.
			status = StatusAbandoned
		}

		if _, err := tx.Exec(
			`UPDATE executions SET status = ?, final_reply = ?, ended_ns = ? WHERE id = ?`,
			status, outcome.Reply, sqlitedb.ToNS(now), ref.id,
		); err != nil {
			return "", "", fmt.Errorf("tasks: recording outcome: %w", err)
		}

		if _, err := tx.Exec(
			`UPDATE tasks SET status = ?, updated_ns = ? WHERE id = ?`, status, sqlitedb.ToNS(now), ref.taskID,
		); err != nil {
			return "", "", fmt.Errorf("tasks: recording outcome: %w", err)
		}

		if err := promoteNext(tx, workerID, now); err != nil {
			return "", "", err
		}
	}

	if err := tx.Commit(); err != nil {
		return "", "", fmt.Errorf("tasks: recording outcome: %w", err)
	}
	if outcome.Kind != OutcomeQuery && outcome.Kind != OutcomeApproval {
		ended = ref.taskID
	}

	return ref.taskID, ref.id, nil
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
	var ended []string
	defer func() { s.tellEnded(workerID, ended...) }() // runs after the unlock below
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := sqlitedb.ToNS(s.now())

	open, err := sqlitedb.QueryAll(tx, func(r sqlitedb.Scanner) (executionRef, error) {
		var ref executionRef
		err := r.Scan(&ref.id, &ref.taskID, &ref.status)
		return ref, err
	}, `SELECT id, task_id, status FROM executions WHERE worker_id = ? AND status IN (?, ?, ?)`,
		workerID, StatusQueued, StatusRunning, StatusWaiting)
	if err != nil {
		return err
	}

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

	if err := tx.Commit(); err != nil {
		return err
	}
	for _, ref := range open {
		ended = append(ended, ref.taskID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// reading
// ---------------------------------------------------------------------------

const taskColumns = `t.id, t.worker_id, COALESCE(w.name, t.worker_id), t.engine, t.request, t.task_type,
	t.status, t.verdict, t.verdict_note, t.verdict_ns, t.created_ns, t.updated_ns,
	(SELECT lx.checked FROM executions lx WHERE lx.task_id = t.id ORDER BY lx.attempt DESC LIMIT 1)`

func scanTask(scanner sqlitedb.Scanner) (Task, error) {
	var task Task
	var verdictAt, created, updated int64
	var checked sql.NullInt64

	if err := scanner.Scan(&task.ID, &task.WorkerID, &task.WorkerName, &task.Engine, &task.Request, &task.Type,
		&task.Status, &task.Verdict, &task.VerdictNote, &verdictAt, &created, &updated, &checked); err != nil {
		return Task{}, err
	}

	if checked.Valid {
		c := checked.Int64 != 0
		task.Checked = &c
	}
	task.Outcome = DisplayOutcome(task.Status, task.Checked)

	task.VerdictAt = sqlitedb.FromNS(verdictAt)
	task.CreatedAt = sqlitedb.FromNS(created)
	task.UpdatedAt = sqlitedb.FromNS(updated)

	return task, nil
}

// maxRecentTasks bounds what RecentForWorker returns however small a limit is asked for.
const maxRecentTasks = 200

// RecentForWorker returns a worker's tasks newest first, at most limit of them
// (a non-positive or oversized limit is capped at maxRecentTasks). It reads the
// tasks table alone - no executions, steps or counts - so a dashboard poll can
// ask for every card's list every few seconds without the cost of a summary.
func (s *Store) RecentForWorker(workerID string, limit int) ([]Task, error) {
	if limit <= 0 || limit > maxRecentTasks {
		limit = maxRecentTasks
	}

	list, err := sqlitedb.QueryAll(s.db, scanTask,
		`SELECT `+taskColumns+` FROM tasks t LEFT JOIN workers w ON w.id = t.worker_id
		 WHERE t.worker_id = ? ORDER BY t.created_ns DESC LIMIT ?`, workerID, limit)
	if err != nil {
		return nil, fmt.Errorf("tasks: listing worker tasks: %w", err)
	}

	if list == nil {
		list = []Task{}
	}

	return list, nil
}

// ShortTitle is a request as one line of at most limit runes, for a list or
// a dropdown that names the task without showing the whole request.
func ShortTitle(request string, limit int) string {
	return clipText(request, limit)
}

// TaskResult returns a task's current status, whether its final check judged
// the result (nil: the desktop did not say) and, once an execution has ended,
// the reply it ended with - what internal/dag polls to learn a dispatched
// node's ticket has finished, and what a later node's output placeholder pulls
// from. ok is false when taskID does not exist.
func (s *Store) TaskResult(taskID string) (status string, checked *bool, finalReply string, ok bool) {
	var c sql.NullInt64
	err := s.db.QueryRow(
		`SELECT status, checked, final_reply FROM executions WHERE task_id = ? ORDER BY attempt DESC LIMIT 1`, taskID,
	).Scan(&status, &c, &finalReply)
	if err != nil {
		return "", nil, "", false
	}
	if c.Valid {
		v := c.Int64 != 0
		checked = &v
	}

	return status, checked, finalReply, true
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

	detail := Detail{Task: task}
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
	defer steps.Close()

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

	detail.Executions = executions

	return detail, nil
}

func (s *Store) loadExecutions(where string, args ...any) ([]Execution, error) {
	executions, err := sqlitedb.QueryAll(s.db, func(r sqlitedb.Scanner) (Execution, error) {
		var execution Execution
		var started, ended int64
		var cost sql.NullFloat64
		var input, output, cached, checked sql.NullInt64
		var verdict string

		if err := r.Scan(&execution.ID, &execution.TaskID, &execution.Attempt, &execution.WorkerID,
			&execution.Engine, &execution.Model, &execution.Status, &execution.FinalReply,
			&started, &ended, &cost, &input, &output, &cached, &checked, &verdict,
			&execution.Origin.Kind, &execution.Origin.Ref, &execution.Origin.ActorID,
			&execution.Skill.Name, &execution.Skill.Version, &execution.ResolvedModel); err != nil {
			return Execution{}, err
		}
		if execution.Origin.Kind == "" {
			execution.Origin.Kind = OriginUnknown
		}
		if verdict != "" {
			var v GateVerdict
			if json.Unmarshal([]byte(verdict), &v) == nil {
				execution.Verdict = &v
			}
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
		if cached.Valid {
			execution.CachedInputTokens = &cached.Int64
		}
		if checked.Valid {
			c := checked.Int64 != 0
			execution.Checked = &c
		}
		execution.Outcome = DisplayOutcome(execution.Status, execution.Checked)

		return execution, nil
	}, `SELECT e.id, e.task_id, e.attempt, e.worker_id, e.engine, e.model, e.status, e.final_reply,
			e.started_ns, e.ended_ns, e.cost_usd, e.input_tokens, e.output_tokens, e.cached_input_tokens, e.checked,
			e.verdict, e.origin, e.origin_ref, e.origin_actor, e.skill_name, e.skill_version, e.resolved_model
		 FROM executions e JOIN tasks t ON t.id = e.task_id `+where+` ORDER BY e.task_id, e.attempt`, args...)
	if err != nil {
		return nil, fmt.Errorf("tasks: reading executions: %w", err)
	}

	return executions, nil
}

func (s *Store) loadInterventions(where string, args ...any) ([]Intervention, error) {
	interventions, err := sqlitedb.QueryAll(s.db, func(r sqlitedb.Scanner) (Intervention, error) {
		var intervention Intervention
		var requested, replied int64

		err := r.Scan(&intervention.ID, &intervention.ExecutionID, &intervention.Kind, &intervention.Reason,
			&intervention.Request, &intervention.Reply, &requested, &replied, &intervention.Secret,
			&intervention.ReplierID, &intervention.JudgementID)

		intervention.RequestedAt = sqlitedb.FromNS(requested)
		intervention.RepliedAt = sqlitedb.FromNS(replied)

		return intervention, err
	}, `SELECT i.id, i.execution_id, i.kind, i.reason, i.request, i.reply, i.requested_ns, i.replied_ns, i.secret,
		i.replier_id, i.judgement_id
		 FROM interventions i JOIN executions e ON e.id = i.execution_id JOIN tasks t ON t.id = e.task_id `+
		where+` ORDER BY i.requested_ns`, args...)
	if err != nil {
		return nil, fmt.Errorf("tasks: reading interventions: %w", err)
	}

	return interventions, nil
}

func normalize(text string) string {
	return strings.Join(strings.Fields(strings.ToLower(text)), " ")
}

// nullable stores a reported figure, or NULL when none was reported.
func nullable[T any](value *T) any {
	if value == nil {
		return nil
	}

	return *value
}

// nullBool stores a said/unsaid flag: 1, 0, or NULL.
func nullBool(value *bool) any {
	if value == nil {
		return nil
	}
	if *value {
		return 1
	}
	return 0
}
