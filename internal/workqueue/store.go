// Package workqueue holds work for the whole workforce rather than for one
// named desktop: items wait in a queue, and a Dispatcher hands each to an
// idle worker that holds the skill or role it needs, within that worker's
// capacity and spend limits (docs/workqueue.md).
package workqueue

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// Item statuses. queued and held wait; assigned and running are in flight;
// the rest are how an item ended.
const (
	StatusQueued    = "queued"
	StatusAssigned  = "assigned"
	StatusRunning   = "running"
	StatusDone      = "done"
	StatusNotDone   = "not_done"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
	StatusHeld      = "held"
)

// Why an item is held: over a spend limit (released by the dispatcher once a
// worker is under its limit again), or interrupted while being sent (released
// only by a person, who must first check whether it reached the desktop).
const (
	HoldBudget      = "budget"
	HoldInterrupted = "interrupted"
)

// Spend periods.
const (
	PeriodDay   = "day"
	PeriodMonth = "month"
)

// AnyWorker is the limit row that applies to every worker without its own.
const AnyWorker = "*"

// Bounds on what one item may carry.
const (
	MaxRequestBytes = 64 << 10
	MaxFiles        = 20
	MaxFileBytes    = 25 << 20 // all of one item's files together
	MaxConcurrent   = 16
	DefaultAttempts = 3
)

var (
	// ErrNotFound is an item that does not exist.
	ErrNotFound = errors.New("workqueue: not found")
	// ErrInvalid is input the store refuses.
	ErrInvalid = errors.New("workqueue: invalid")
	// ErrConflict is a change the item's current status does not allow.
	ErrConflict = errors.New("workqueue: not allowed in this status")
)

var (
	workerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
	namePattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _.-]{0,63}$`)
	filePattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _.()+-]{0,127}$`)
)

// invalid wraps ErrInvalid with a reason fit to show the caller.
func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Reason returns the caller-facing part of an ErrInvalid, "" for any other error.
func Reason(err error) string {
	if !errors.Is(err, ErrInvalid) {
		return ""
	}
	return strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": ")
}

// ValidWorker reports whether id has the shape of a worker id.
func ValidWorker(id string) bool { return workerPattern.MatchString(id) }

var schema = []string{
	`CREATE TABLE workqueue_items (
		id              TEXT PRIMARY KEY,
		request         TEXT NOT NULL,
		required_skill  TEXT NOT NULL DEFAULT '',
		required_role   TEXT NOT NULL DEFAULT '',
		worker          TEXT NOT NULL DEFAULT '',
		priority        INTEGER NOT NULL DEFAULT 0,
		not_before_ns   INTEGER NOT NULL DEFAULT 0,
		status          TEXT NOT NULL,
		hold            TEXT NOT NULL DEFAULT '',
		assigned_worker TEXT NOT NULL DEFAULT '',
		task_id         TEXT NOT NULL DEFAULT '',
		attempts        INTEGER NOT NULL DEFAULT 0,
		max_attempts    INTEGER NOT NULL DEFAULT 3,
		reason          TEXT NOT NULL DEFAULT '',
		source          TEXT NOT NULL DEFAULT '',
		created_ns      INTEGER NOT NULL,
		updated_ns      INTEGER NOT NULL
	)`,
	`CREATE INDEX workqueue_items_status ON workqueue_items (status, priority DESC, created_ns)`,
	`CREATE TABLE workqueue_files (
		item_id TEXT NOT NULL REFERENCES workqueue_items (id) ON DELETE CASCADE,
		name    TEXT NOT NULL,
		data    BLOB NOT NULL,
		PRIMARY KEY (item_id, name)
	)`,
	`CREATE TABLE workqueue_capacity (
		worker         TEXT PRIMARY KEY,
		max_concurrent INTEGER NOT NULL DEFAULT 1,
		paused         INTEGER NOT NULL DEFAULT 0,
		updated_ns     INTEGER NOT NULL
	)`,
	`CREATE TABLE workqueue_limits (
		worker     TEXT NOT NULL,
		period     TEXT NOT NULL,
		max_usd    REAL NOT NULL DEFAULT 0,
		max_tokens INTEGER NOT NULL DEFAULT 0,
		updated_ns INTEGER NOT NULL,
		PRIMARY KEY (worker, period)
	)`,
	// The steps above are applied on every existing installation and must
	// never be edited - schema changes only ever append.
}

// Item is one piece of work in the queue.
type Item struct {
	ID             string     `json:"id"`
	Request        string     `json:"request"`
	Files          []FileInfo `json:"files"`
	RequiredSkill  string     `json:"required_skill,omitempty"`
	RequiredRole   string     `json:"required_role,omitempty"`
	Worker         string     `json:"worker,omitempty"`
	Priority       int        `json:"priority"`
	NotBefore      time.Time  `json:"not_before"`
	Status         string     `json:"status"`
	Hold           string     `json:"hold,omitempty"`
	AssignedWorker string     `json:"assigned_worker,omitempty"`
	TaskID         string     `json:"task_id,omitempty"`
	Attempts       int        `json:"attempts"`
	MaxAttempts    int        `json:"max_attempts"`
	Reason         string     `json:"reason"`
	Source         string     `json:"source,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// FileInfo is a file an item carries, without its bytes.
type FileInfo struct {
	Name string `json:"name"`
	Size int    `json:"size"`
}

// File is a file an item carries to its worker's desktop.
type File struct {
	Name string `json:"name"`
	Data []byte `json:"data"`
}

// NewItem is what Enqueue takes.
type NewItem struct {
	Request       string    `json:"request"`
	Files         []File    `json:"files"`
	RequiredSkill string    `json:"required_skill"`
	RequiredRole  string    `json:"required_role"`
	Worker        string    `json:"worker"`
	Priority      int       `json:"priority"`
	NotBefore     time.Time `json:"not_before"`
	MaxAttempts   int       `json:"max_attempts"`
	Source        string    `json:"source"`
}

// Capacity is how much a worker takes from the queue at once.
type Capacity struct {
	Worker        string    `json:"worker"`
	MaxConcurrent int       `json:"max_concurrent"`
	Paused        bool      `json:"paused"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Limit caps one worker's (or, with Worker "*", each worker's) spend per
// period. A zero MaxUSD or MaxTokens leaves that measure uncapped.
type Limit struct {
	Worker    string    `json:"worker"`
	Period    string    `json:"period"`
	MaxUSD    float64   `json:"max_usd"`
	MaxTokens int64     `json:"max_tokens"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Filter narrows List.
type Filter struct {
	Status string
	Worker string // the pinned or assigned worker
	Limit  int
}

// Store is the SQLite-backed work queue.
type Store struct {
	db *sql.DB
	// mu serialises read-then-write sequences (status moves that check the
	// current status first, enqueue with its files).
	mu  sync.Mutex
	now func() time.Time
}

// NewStore opens the work queue on db, creating its tables if needed.
func NewStore(db *sql.DB) (*Store, error) {
	if err := sqlitedb.Migrate(db, "workqueue", schema); err != nil {
		return nil, err
	}
	return &Store{db: db, now: func() time.Time { return time.Now().UTC() }}, nil
}

// SetClock replaces the store's clock, for tests.
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// validName checks an optional skill or role name.
func validName(kind, v string) error {
	if v != "" && !namePattern.MatchString(v) {
		return invalid("not a %s name", kind)
	}
	return nil
}

// Enqueue validates and stores a new queued item.
func (s *Store) Enqueue(in NewItem) (Item, error) {
	in.Request = strings.TrimSpace(in.Request)
	in.RequiredSkill = strings.TrimSpace(in.RequiredSkill)
	in.RequiredRole = strings.TrimSpace(in.RequiredRole)
	in.Worker = strings.TrimSpace(in.Worker)
	in.Source = strings.TrimSpace(in.Source)

	switch {
	case in.Request == "":
		return Item{}, invalid("request is empty")
	case len(in.Request) > MaxRequestBytes:
		return Item{}, invalid("request is longer than %d bytes", MaxRequestBytes)
	case in.Worker != "" && !ValidWorker(in.Worker):
		return Item{}, invalid("not a worker id")
	case len(in.Files) > MaxFiles:
		return Item{}, invalid("at most %d files", MaxFiles)
	case len(in.Source) > 128:
		return Item{}, invalid("source is too long")
	case in.MaxAttempts < 0 || in.MaxAttempts > 10:
		return Item{}, invalid("max_attempts must be 1-10")
	}
	if err := validName("skill", in.RequiredSkill); err != nil {
		return Item{}, err
	}
	if err := validName("role", in.RequiredRole); err != nil {
		return Item{}, err
	}
	if in.MaxAttempts == 0 {
		in.MaxAttempts = DefaultAttempts
	}

	total, seen := 0, map[string]bool{}
	for _, f := range in.Files {
		if !filePattern.MatchString(f.Name) || strings.Contains(f.Name, "..") {
			return Item{}, invalid("not a plain file name: %q", f.Name)
		}
		if seen[f.Name] {
			return Item{}, invalid("file %q given twice", f.Name)
		}
		seen[f.Name] = true
		total += len(f.Data)
	}
	if total > MaxFileBytes {
		return Item{}, invalid("files are larger than %d bytes together", MaxFileBytes)
	}

	now := s.now()
	id := sqlitedb.NewID("q", 8)

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return Item{}, fmt.Errorf("workqueue: enqueue: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`INSERT INTO workqueue_items (id, request, required_skill, required_role, worker, priority, not_before_ns,
			status, max_attempts, source, created_ns, updated_ns) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, in.Request, in.RequiredSkill, in.RequiredRole, in.Worker, in.Priority, sqlitedb.ToNS(in.NotBefore),
		StatusQueued, in.MaxAttempts, in.Source, sqlitedb.ToNS(now), sqlitedb.ToNS(now),
	); err != nil {
		return Item{}, fmt.Errorf("workqueue: enqueue: %w", err)
	}
	for _, f := range in.Files {
		if _, err := tx.Exec(`INSERT INTO workqueue_files (item_id, name, data) VALUES (?, ?, ?)`, id, f.Name, f.Data); err != nil {
			return Item{}, fmt.Errorf("workqueue: enqueue: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Item{}, fmt.Errorf("workqueue: enqueue: %w", err)
	}

	return s.getLocked(id)
}

const itemColumns = `id, request, required_skill, required_role, worker, priority, not_before_ns, status, hold,
	assigned_worker, task_id, attempts, max_attempts, reason, source, created_ns, updated_ns`

// scanItem reads one item row (without its files).
func scanItem(r sqlitedb.Scanner) (Item, error) {
	var it Item
	var notBefore, created, updated int64
	if err := r.Scan(&it.ID, &it.Request, &it.RequiredSkill, &it.RequiredRole, &it.Worker, &it.Priority, &notBefore,
		&it.Status, &it.Hold, &it.AssignedWorker, &it.TaskID, &it.Attempts, &it.MaxAttempts, &it.Reason, &it.Source,
		&created, &updated); err != nil {
		return Item{}, err
	}
	it.NotBefore = sqlitedb.FromNS(notBefore)
	it.CreatedAt = sqlitedb.FromNS(created)
	it.UpdatedAt = sqlitedb.FromNS(updated)
	it.Files = []FileInfo{}
	return it, nil
}

// scanFileInfo reads one (item id, file info) row.
func scanFileInfo(r sqlitedb.Scanner) (struct {
	item string
	info FileInfo
}, error) {
	var out struct {
		item string
		info FileInfo
	}
	err := r.Scan(&out.item, &out.info.Name, &out.info.Size)
	return out, err
}

// attachFiles fills each item's file list, after the item rows are closed.
func (s *Store) attachFiles(items []Item) error {
	if len(items) == 0 {
		return nil
	}
	byID := map[string]*Item{}
	args := make([]any, len(items))
	marks := make([]string, len(items))
	for i := range items {
		byID[items[i].ID] = &items[i]
		args[i] = items[i].ID
		marks[i] = "?"
	}
	rows, err := sqlitedb.QueryAll(s.db, scanFileInfo,
		`SELECT item_id, name, length(data) FROM workqueue_files WHERE item_id IN (`+strings.Join(marks, ",")+`) ORDER BY name`, args...)
	if err != nil {
		return fmt.Errorf("workqueue: reading files: %w", err)
	}
	for _, r := range rows {
		if it := byID[r.item]; it != nil {
			it.Files = append(it.Files, r.info)
		}
	}
	return nil
}

// Get returns one item.
func (s *Store) Get(id string) (Item, error) {
	return s.getLocked(id)
}

// getLocked reads one item; safe with or without mu held (it only reads).
func (s *Store) getLocked(id string) (Item, error) {
	it, err := scanItem(s.db.QueryRow(`SELECT `+itemColumns+` FROM workqueue_items WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, ErrNotFound
	}
	if err != nil {
		return Item{}, fmt.Errorf("workqueue: reading item: %w", err)
	}
	items := []Item{it}
	if err := s.attachFiles(items); err != nil {
		return Item{}, err
	}
	return items[0], nil
}

// List returns items, newest first, narrowed by f.
func (s *Store) List(f Filter) ([]Item, error) {
	where, args := []string{"1=1"}, []any{}
	if f.Status != "" {
		where = append(where, "status = ?")
		args = append(args, f.Status)
	}
	if f.Worker != "" {
		where = append(where, "(worker = ? OR assigned_worker = ?)")
		args = append(args, f.Worker, f.Worker)
	}
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 200
	}
	args = append(args, f.Limit)
	items, err := sqlitedb.QueryAll(s.db, scanItem,
		`SELECT `+itemColumns+` FROM workqueue_items WHERE `+strings.Join(where, " AND ")+` ORDER BY created_ns DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("workqueue: listing: %w", err)
	}
	if err := s.attachFiles(items); err != nil {
		return nil, err
	}
	return items, nil
}

// inStatus returns every item in one of statuses, highest priority first,
// then oldest first: the order the dispatcher considers them in.
func (s *Store) inStatus(statuses ...string) ([]Item, error) {
	marks := make([]string, len(statuses))
	args := make([]any, len(statuses))
	for i, st := range statuses {
		marks[i], args[i] = "?", st
	}
	items, err := sqlitedb.QueryAll(s.db, scanItem,
		`SELECT `+itemColumns+` FROM workqueue_items WHERE status IN (`+strings.Join(marks, ",")+`)
		 ORDER BY priority DESC, created_ns, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("workqueue: reading queue: %w", err)
	}
	return items, nil
}

// Files returns the files an item carries, with their bytes.
func (s *Store) Files(id string) ([]File, error) {
	files, err := sqlitedb.QueryAll(s.db, func(r sqlitedb.Scanner) (File, error) {
		var f File
		err := r.Scan(&f.Name, &f.Data)
		return f, err
	}, `SELECT name, data FROM workqueue_files WHERE item_id = ? ORDER BY name`, id)
	if err != nil {
		return nil, fmt.Errorf("workqueue: reading files: %w", err)
	}
	return files, nil
}

// move changes an item's status when its current status is one of from, and
// reports whether it did. set holds extra "column = ?" assignments.
func (s *Store) move(id string, from []string, to string, set map[string]any) (bool, error) {
	cols := []string{"status = ?", "updated_ns = ?"}
	args := []any{to, sqlitedb.ToNS(s.now())}
	for _, col := range []string{"hold", "assigned_worker", "task_id", "reason", "not_before_ns"} {
		if v, ok := set[col]; ok {
			cols = append(cols, col+" = ?")
			args = append(args, v)
		}
	}
	if _, ok := set["attempts+1"]; ok {
		cols = append(cols, "attempts = attempts + 1")
	}
	args = append(args, id)
	marks := make([]string, len(from))
	for i, st := range from {
		marks[i] = "?"
		args = append(args, st)
	}
	res, err := s.db.Exec(`UPDATE workqueue_items SET `+strings.Join(cols, ", ")+` WHERE id = ? AND status IN (`+strings.Join(marks, ",")+`)`, args...)
	if err != nil {
		return false, fmt.Errorf("workqueue: updating item: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("workqueue: updating item: %w", err)
	}
	return n == 1, nil
}

// transition is move for a caller who needs the item back, with
// ErrNotFound/ErrConflict when it could not be moved.
func (s *Store) transition(id string, from []string, to string, set map[string]any) (Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ok, err := s.move(id, from, to, set)
	if err != nil {
		return Item{}, err
	}
	it, err := s.getLocked(id)
	if err != nil {
		return Item{}, err
	}
	if !ok {
		return it, ErrConflict
	}
	return it, nil
}

// Cancel stops an item that has not ended. A running item's task keeps
// running on its desktop - the queue only stops tracking it.
func (s *Store) Cancel(id, reason string) (Item, error) {
	if reason = strings.TrimSpace(reason); reason == "" {
		reason = "cancelled"
	}
	return s.transition(id, []string{StatusQueued, StatusHeld, StatusAssigned, StatusRunning}, StatusCancelled,
		map[string]any{"reason": truncate(reason, 500), "hold": ""})
}

// Retry puts an ended or held item back in the queue for any eligible worker
// (or its pinned one). A done item is never retried.
func (s *Store) Retry(id string) (Item, error) {
	return s.transition(id, []string{StatusNotDone, StatusFailed, StatusCancelled, StatusHeld}, StatusQueued,
		map[string]any{"reason": "retried by a person", "hold": "", "assigned_worker": "", "task_id": "", "not_before_ns": int64(0)})
}

// claim moves a queued item to assigned on worker; false when another
// change got there first.
func (s *Store) claim(id, worker string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.move(id, []string{StatusQueued}, StatusAssigned,
		map[string]any{"assigned_worker": worker, "reason": "sending to " + worker, "attempts+1": true})
}

// started records the task an assigned item landed under.
func (s *Store) started(id, taskID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.move(id, []string{StatusAssigned}, StatusRunning, map[string]any{"task_id": taskID, "reason": ""})
}

// unassign returns an assigned item to the queue (the send was refused).
// A busy refusal gives the attempt back; any other refusal keeps it counted.
func (s *Store) unassign(id, reason string, notBefore time.Time, refund bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if refund {
		if _, err := s.db.Exec(`UPDATE workqueue_items SET attempts = MAX(attempts - 1, 0) WHERE id = ? AND status = ?`, id, StatusAssigned); err != nil {
			return fmt.Errorf("workqueue: updating item: %w", err)
		}
	}
	_, err := s.move(id, []string{StatusAssigned}, StatusQueued,
		map[string]any{"assigned_worker": "", "reason": truncate(reason, 500), "not_before_ns": sqlitedb.ToNS(notBefore)})
	return err
}

// finish ends an in-flight item.
func (s *Store) finish(id, status, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.move(id, []string{StatusAssigned, StatusRunning}, status, map[string]any{"reason": truncate(reason, 500)})
	return err
}

// hold parks an item that is waiting or interrupted, with why.
func (s *Store) hold(id string, from []string, kind, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.move(id, from, StatusHeld, map[string]any{"hold": kind, "reason": truncate(reason, 500)})
	return err
}

// release returns a budget-held item to the queue.
func (s *Store) release(id, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE workqueue_items SET status = ?, hold = '', reason = ?, updated_ns = ? WHERE id = ? AND status = ? AND hold = ?`,
		StatusQueued, truncate(reason, 500), sqlitedb.ToNS(s.now()), id, StatusHeld, HoldBudget)
	if err != nil {
		return fmt.Errorf("workqueue: updating item: %w", err)
	}
	return nil
}

// note sets the reason a queued or running item shows, when it changed.
func (s *Store) note(id, status, reason string) error {
	_, err := s.db.Exec(`UPDATE workqueue_items SET reason = ?, updated_ns = ? WHERE id = ? AND status = ? AND reason != ?`,
		truncate(reason, 500), sqlitedb.ToNS(s.now()), id, status, truncate(reason, 500))
	if err != nil {
		return fmt.Errorf("workqueue: updating item: %w", err)
	}
	return nil
}

// SetCapacity stores a worker's capacity.
func (s *Store) SetCapacity(c Capacity) (Capacity, error) {
	if !ValidWorker(c.Worker) {
		return Capacity{}, invalid("not a worker id")
	}
	if c.MaxConcurrent == 0 {
		c.MaxConcurrent = 1
	}
	if c.MaxConcurrent < 1 || c.MaxConcurrent > MaxConcurrent {
		return Capacity{}, invalid("max_concurrent must be 1-%d", MaxConcurrent)
	}
	c.UpdatedAt = s.now()
	paused := 0
	if c.Paused {
		paused = 1
	}
	if _, err := s.db.Exec(
		`INSERT INTO workqueue_capacity (worker, max_concurrent, paused, updated_ns) VALUES (?, ?, ?, ?)
		 ON CONFLICT(worker) DO UPDATE SET max_concurrent = excluded.max_concurrent, paused = excluded.paused,
			updated_ns = excluded.updated_ns`,
		c.Worker, c.MaxConcurrent, paused, sqlitedb.ToNS(c.UpdatedAt)); err != nil {
		return Capacity{}, fmt.Errorf("workqueue: setting capacity: %w", err)
	}
	return c, nil
}

// Capacities returns every stored capacity by worker; a worker without one
// takes one item at a time and is not paused.
func (s *Store) Capacities() (map[string]Capacity, error) {
	rows, err := sqlitedb.QueryAll(s.db, func(r sqlitedb.Scanner) (Capacity, error) {
		var c Capacity
		var paused int
		var updated int64
		err := r.Scan(&c.Worker, &c.MaxConcurrent, &paused, &updated)
		c.Paused, c.UpdatedAt = paused != 0, sqlitedb.FromNS(updated)
		return c, err
	}, `SELECT worker, max_concurrent, paused, updated_ns FROM workqueue_capacity ORDER BY worker`)
	if err != nil {
		return nil, fmt.Errorf("workqueue: reading capacity: %w", err)
	}
	out := make(map[string]Capacity, len(rows))
	for _, c := range rows {
		out[c.Worker] = c
	}
	return out, nil
}

// capacityOf is a worker's capacity, the default when none is stored.
func capacityOf(all map[string]Capacity, worker string) Capacity {
	if c, ok := all[worker]; ok {
		return c
	}
	return Capacity{Worker: worker, MaxConcurrent: 1}
}

// SetLimit stores a spend limit; one with no cap at all removes it.
func (s *Store) SetLimit(l Limit) (Limit, error) {
	if l.Worker != AnyWorker && !ValidWorker(l.Worker) {
		return Limit{}, invalid(`worker must be a worker id or "*"`)
	}
	if l.Period != PeriodDay && l.Period != PeriodMonth {
		return Limit{}, invalid("period must be day or month")
	}
	if l.MaxUSD < 0 || l.MaxTokens < 0 || math.IsNaN(l.MaxUSD) || math.IsInf(l.MaxUSD, 0) {
		return Limit{}, invalid("limits cannot be negative")
	}
	if l.MaxUSD == 0 && l.MaxTokens == 0 {
		if _, err := s.db.Exec(`DELETE FROM workqueue_limits WHERE worker = ? AND period = ?`, l.Worker, l.Period); err != nil {
			return Limit{}, fmt.Errorf("workqueue: removing limit: %w", err)
		}
		return l, nil
	}
	l.UpdatedAt = s.now()
	if _, err := s.db.Exec(
		`INSERT INTO workqueue_limits (worker, period, max_usd, max_tokens, updated_ns) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(worker, period) DO UPDATE SET max_usd = excluded.max_usd, max_tokens = excluded.max_tokens,
			updated_ns = excluded.updated_ns`,
		l.Worker, l.Period, l.MaxUSD, l.MaxTokens, sqlitedb.ToNS(l.UpdatedAt)); err != nil {
		return Limit{}, fmt.Errorf("workqueue: setting limit: %w", err)
	}
	return l, nil
}

// Limits returns every stored spend limit.
func (s *Store) Limits() ([]Limit, error) {
	rows, err := sqlitedb.QueryAll(s.db, func(r sqlitedb.Scanner) (Limit, error) {
		var l Limit
		var updated int64
		err := r.Scan(&l.Worker, &l.Period, &l.MaxUSD, &l.MaxTokens, &updated)
		l.UpdatedAt = sqlitedb.FromNS(updated)
		return l, err
	}, `SELECT worker, period, max_usd, max_tokens, updated_ns FROM workqueue_limits ORDER BY worker, period`)
	if err != nil {
		return nil, fmt.Errorf("workqueue: reading limits: %w", err)
	}
	return rows, nil
}

// limitFor is the limit that applies to worker for period: its own, else
// the "*" one; ok is false when neither exists.
func limitFor(all []Limit, worker, period string) (Limit, bool) {
	var fallback Limit
	found := false
	for _, l := range all {
		if l.Period != period {
			continue
		}
		if l.Worker == worker {
			return l, true
		}
		if l.Worker == AnyWorker {
			fallback, found = l, true
		}
	}
	return fallback, found
}

// truncate cuts s to at most n bytes on a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n]
}
