// Package conversation holds the chat transcript between a human and each
// desktop's agent: the finished requests and replies both sides exchanged,
// plus the live step-by-step activity of whichever turn is running right now.
//
// It is the control plane's own record, not the agent's. A desktop's agent
// still keeps its own reasoning state inside the container (the Ollama loop's
// history list, Claude Code's session, TheBot's L1/L2); this is what the chat
// UI renders, shared by every browser pointed at the same control plane
// instead of living in one tab's localStorage.
//
// Threads are served from memory - the chat's event stream re-reads a whole
// snapshot on every change - and every change is written through to SQLite,
// so a restarted control plane loads each thread back on first use.
package conversation

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/YKoustubhRao/flowera/internal/sqlitedb"
)

// Roles an entry can come from.
const (
	RoleUser  = "user"
	RoleAgent = "agent"
)

// Kinds an agent entry can be: a normal reply, a request for human approval
// or a query (the agent is blocked until a reply arrives), or a failure.
const (
	KindEntry    = "entry"
	KindApproval = "approval"
	KindError    = "error"
)

// maxEntries and maxSteps bound how much of one desktop's chat is retained.
// A long-lived desktop chatting all day must not grow its thread without
// limit; the oldest entries are dropped once these are exceeded. The task
// records (internal/tasks) are the durable history - the chat is the view.
const (
	maxEntries = 200
	maxSteps   = 400
)

// lateStepWindow is how long after an agent's reply a step is still taken as
// part of the turn that reply closed. A desktop reports steps from a
// background queue but its reply directly, so the last line of a turn can
// land after the answer — measured on TheBot, under a millisecond after it.
// Opening a new turn for that line left the chat showing "Working…" forever
// over a finished task.
const lateStepWindow = 10 * time.Second

// Step is one line of live activity from the turn currently running: a shell
// command the agent chose, a file it wrote, a page it read. Every harness
// reports these through the same POST /api/agent/events endpoint, so the chat
// renders all three identically.
type Step struct {
	ID        string    `json:"id"`
	Text      string    `json:"text"`
	Action    *Action   `json:"action,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Action is what makes a step part of the audit trail: one thing an agent did
// on its desktop's screen, reported by the shared action layer inside the
// desktop (flowera_agent/computer) whichever engine asked for it. Before and
// After are URLs of the screenshots either side of it; a stopped action
// (Outcome "needs_approval") has only a Before.
type Action struct {
	// RecordID is the desktop's own audit record for this action, which keeps
	// the full-resolution frames on the desktop's volume.
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

// Entry is one finished turn in the transcript - a human's request or an
// agent's reply. An agent entry carries the Steps that produced it, so a
// completed turn can still be expanded to show the work behind the answer.
type Entry struct {
	ID        string `json:"id"`
	DesktopID string `json:"desktop_id"`
	Role      string `json:"role"`
	Kind      string `json:"kind"`
	Content   string `json:"content"`

	// TaskID links the entry to its task record, and Verdict is the human's
	// "correct"/"wrong" on that task's answer - shown on every device, which
	// is why it travels with the transcript rather than living in one tab.
	TaskID  string `json:"task_id,omitempty"`
	Verdict string `json:"verdict,omitempty"`

	Steps     []Step    `json:"steps,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Thread is a point-in-time snapshot of one desktop's chat, which is what both
// the JSON endpoint and each SSE frame send. Version increments on every
// change, letting a client cheaply tell whether anything actually moved.
type Thread struct {
	DesktopID string    `json:"desktop_id"`
	Entries   []Entry   `json:"entries"`
	Steps     []Step    `json:"steps"`
	Running   bool      `json:"running"`
	StartedAt time.Time `json:"started_at,omitempty"`
	Version   uint64    `json:"version"`
}

// thread is one desktop's mutable state. Its subscribers are notified (never
// blocked) whenever anything changes, so an SSE client can re-read a snapshot.
type thread struct {
	entries     []Entry
	steps       []Step
	running     bool
	startedAt   time.Time
	version     uint64
	subscribers map[chan struct{}]bool
}

var schema = []string{
	`CREATE TABLE chat_threads (
		desktop_id TEXT PRIMARY KEY,
		running    INTEGER NOT NULL,
		started_ns INTEGER NOT NULL
	)`,
	`CREATE TABLE chat_messages (
		id         TEXT PRIMARY KEY,
		desktop_id TEXT NOT NULL,
		role       TEXT NOT NULL,
		kind       TEXT NOT NULL,
		content    TEXT NOT NULL,
		task_id    TEXT NOT NULL DEFAULT '',
		verdict    TEXT NOT NULL DEFAULT '',
		created_ns INTEGER NOT NULL
	)`,
	`CREATE INDEX chat_messages_desktop ON chat_messages (desktop_id)`,
	// message_id '' is the turn still running.
	`CREATE TABLE chat_steps (
		id         TEXT PRIMARY KEY,
		desktop_id TEXT NOT NULL,
		message_id TEXT NOT NULL DEFAULT '',
		text       TEXT NOT NULL,
		action     TEXT NOT NULL DEFAULT '',
		created_ns INTEGER NOT NULL
	)`,
	`CREATE INDEX chat_steps_desktop ON chat_steps (desktop_id, message_id)`,
	// The steps above are applied on every existing installation and must
	// never be edited - schema changes only ever append. This step renames
	// chat_messages/its index and chat_steps' message_id column to match the
	// entry/request/reply/query vocabulary the rest of the app now uses.
	// SQLite's RENAME COLUMN propagates into chat_steps_desktop's own
	// definition automatically, so that index needs no separate step.
	`ALTER TABLE chat_messages RENAME TO chat_entries`,
	`DROP INDEX chat_messages_desktop`,
	`CREATE INDEX chat_entries_desktop ON chat_entries (desktop_id)`,
	`ALTER TABLE chat_steps RENAME COLUMN message_id TO entry_id`,
}

// Store is a set of per-desktop chat threads, held in memory and written
// through to SQLite.
type Store struct {
	mu      sync.Mutex
	db      *sql.DB
	threads map[string]*thread
	seq     uint64
}

// NewStore opens the conversation Store on db, creating its tables if needed.
func NewStore(db *sql.DB) (*Store, error) {
	if err := sqlitedb.Migrate(db, "conversation", schema); err != nil {
		return nil, err
	}

	return &Store{
		db:      db,
		threads: make(map[string]*thread),
	}, nil
}

// threadFor returns the thread for a desktop, loading it from the database on
// first use. The caller must hold the lock.
func (s *Store) threadFor(desktopID string) *thread {
	existing, ok := s.threads[desktopID]
	if ok {
		return existing
	}

	loaded := s.load(desktopID)
	s.threads[desktopID] = loaded

	return loaded
}

// nextID returns a unique identifier for an entry or step. The caller must
// hold the lock. A plain timestamp is not enough on its own: several steps can
// land within the same nanosecond tick, and the UI keys on these.
func (s *Store) nextID() string {
	s.seq++

	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), s.seq)
}

// touch bumps the thread's version and wakes its subscribers. The caller must
// hold the lock.
func (t *thread) touch() {
	t.version++

	for subscriber := range t.subscribers {
		select {
		case subscriber <- struct{}{}:
		default:
			// Already has an unread wake-up pending; one is enough,
			// since a subscriber re-reads the whole snapshot anyway.
		}
	}
}

// AppendUserRequest records something the human sent to a desktop and opens a
// new turn: the agent is now considered busy, and the steps it reports from
// here on belong to this turn.
func (s *Store) AppendUserRequest(desktopID, taskID, content string) Entry {
	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.threadFor(desktopID)

	entry := Entry{
		ID:        s.nextID(),
		DesktopID: desktopID,
		Role:      RoleUser,
		Kind:      KindEntry,
		Content:   content,
		TaskID:    taskID,
		CreatedAt: time.Now().UTC(),
	}

	var dropped []Entry

	current.entries, dropped = appendCapped(current.entries, entry, maxEntries)

	// A request sent while the desktop is already working on an earlier one
	// (queued behind it, or absorbed as its reply if that task pauses - see
	// watchers.command_watcher/absorbQueuedReply) does not start a fresh
	// turn: the live steps already reported for the turn in flight are real,
	// ongoing work and must not be wiped out from under it.
	startingNewTurn := !current.running
	if startingNewTurn {
		current.steps = nil
		current.startedAt = entry.CreatedAt
	}
	current.running = true

	s.persist(desktopID, current, func(tx *sql.Tx) error {
		if err := insertEntry(tx, entry); err != nil {
			return err
		}

		if startingNewTurn {
			if _, err := tx.Exec(`DELETE FROM chat_steps WHERE desktop_id = ? AND entry_id = ''`, desktopID); err != nil {
				return err
			}
		}

		return deleteEntries(tx, dropped)
	})

	current.touch()

	return entry
}

// AppendAgentReply records the agent's reply and closes the current turn,
// attaching the steps it took along the way to the entry itself.
func (s *Store) AppendAgentReply(desktopID, taskID, kind, content string) Entry {
	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.threadFor(desktopID)

	switch kind {
	case KindEntry, KindApproval, KindError:
	default:
		kind = KindEntry
	}

	entry := Entry{
		ID:        s.nextID(),
		DesktopID: desktopID,
		Role:      RoleAgent,
		Kind:      kind,
		Content:   content,
		TaskID:    taskID,
		Steps:     current.steps,
		CreatedAt: time.Now().UTC(),
	}

	var dropped []Entry

	current.entries, dropped = appendCapped(current.entries, entry, maxEntries)
	current.steps = nil
	current.running = false
	current.startedAt = time.Time{}

	s.persist(desktopID, current, func(tx *sql.Tx) error {
		if err := insertEntry(tx, entry); err != nil {
			return err
		}

		if _, err := tx.Exec(
			`UPDATE chat_steps SET entry_id = ? WHERE desktop_id = ? AND entry_id = ''`,
			entry.ID, desktopID,
		); err != nil {
			return err
		}

		return deleteEntries(tx, dropped)
	})

	current.touch()

	return entry
}

// AppendStep records one line of live activity for the running turn. A step
// arriving while nothing is marked running still opens a turn, so work a
// desktop started on its own is visible rather than silently dropped — unless
// it trails a turn that only just closed, in which case it belongs to that one.
func (s *Store) AppendStep(desktopID, text string) Step {
	return s.appendStep(desktopID, text, nil)
}

// AppendActionStep records one on-screen action as a step of the running turn,
// under the same rules as AppendStep.
func (s *Store) AppendActionStep(desktopID, text string, action Action) Step {
	return s.appendStep(desktopID, text, &action)
}

// appendStep is AppendStep and AppendActionStep's shared body.
func (s *Store) appendStep(desktopID, text string, action *Action) Step {
	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.threadFor(desktopID)

	step := Step{
		ID:        s.nextID(),
		Text:      text,
		Action:    action,
		CreatedAt: time.Now().UTC(),
	}

	var dropped []Step

	if last := current.lastEntry(); !current.running && last != nil &&
		last.Role == RoleAgent && step.CreatedAt.Sub(last.CreatedAt) < lateStepWindow {
		last.Steps, dropped = appendCapped(last.Steps, step, maxSteps)

		s.persist(desktopID, current, func(tx *sql.Tx) error {
			if err := insertStep(tx, desktopID, last.ID, step); err != nil {
				return err
			}

			return deleteSteps(tx, dropped)
		})

		current.touch()

		return step
	}

	if !current.running {
		current.running = true
		current.startedAt = step.CreatedAt
	}

	current.steps, dropped = appendCapped(current.steps, step, maxSteps)

	s.persist(desktopID, current, func(tx *sql.Tx) error {
		if err := insertStep(tx, desktopID, "", step); err != nil {
			return err
		}

		return deleteSteps(tx, dropped)
	})

	current.touch()

	return step
}

// SetVerdict records the human's verdict ("correct", "wrong", or "" to take
// it back) on every agent entry belonging to a task.
func (s *Store) SetVerdict(desktopID, taskID, verdict string) {
	if taskID == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.threadFor(desktopID)

	for i := range current.entries {
		if current.entries[i].TaskID == taskID && current.entries[i].Role == RoleAgent {
			current.entries[i].Verdict = verdict
		}
	}

	s.persist(desktopID, current, func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`UPDATE chat_entries SET verdict = ? WHERE desktop_id = ? AND task_id = ? AND role = ?`,
			verdict, desktopID, taskID, RoleAgent,
		)
		return err
	})

	current.touch()
}

// Snapshot returns a copy of one desktop's thread, safe for a caller to hold
// and serialize after the lock is released.
func (s *Store) Snapshot(desktopID string) Thread {
	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.threadFor(desktopID)

	entries := make([]Entry, len(current.entries))
	copy(entries, current.entries)

	steps := make([]Step, len(current.steps))
	copy(steps, current.steps)

	return Thread{
		DesktopID: desktopID,
		Entries:   entries,
		Steps:     steps,
		Running:   current.running,
		StartedAt: current.startedAt,
		Version:   current.version,
	}
}

// Clear discards one desktop's transcript, including any turn it currently
// believes is running. The task records behind it are kept.
func (s *Store) Clear(desktopID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.threadFor(desktopID)

	current.entries = nil
	current.steps = nil
	current.running = false
	current.startedAt = time.Time{}

	s.persist(desktopID, current, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM chat_steps WHERE desktop_id = ?`, desktopID); err != nil {
			return err
		}

		_, err := tx.Exec(`DELETE FROM chat_entries WHERE desktop_id = ?`, desktopID)
		return err
	})

	current.touch()
}

// Subscribe returns a channel that receives a wake-up whenever a desktop's
// thread changes, plus the function to call to stop listening. The channel is
// buffered and sent to non-blocking, so a slow reader never stalls a writer.
func (s *Store) Subscribe(desktopID string) (<-chan struct{}, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.threadFor(desktopID)

	updates := make(chan struct{}, 1)
	current.subscribers[updates] = true

	return updates, func() {
		s.mu.Lock()
		defer s.mu.Unlock()

		delete(current.subscribers, updates)
	}
}

// lastEntry returns the thread's most recent entry, or nil for an empty
// thread. The caller must hold the lock.
func (t *thread) lastEntry() *Entry {
	if len(t.entries) == 0 {
		return nil
	}

	return &t.entries[len(t.entries)-1]
}

// appendCapped appends an item, dropping the oldest entries once the slice
// would exceed max, and returns what it dropped.
func appendCapped[T any](items []T, item T, max int) (kept []T, dropped []T) {
	items = append(items, item)

	if len(items) > max {
		dropped = append(dropped, items[:len(items)-max]...)
		items = items[len(items)-max:]
	}

	return items, dropped
}

// ---------------------------------------------------------------------------
// persistence
// ---------------------------------------------------------------------------

// persist runs one change's writes, plus the thread's running state, in a
// single transaction. The caller must hold the lock, which keeps the database
// in the same order as memory. A failed write is logged rather than returned:
// the in-memory thread is still correct for everyone watching it, and a chat
// must not stop working because the disk did.
func (s *Store) persist(desktopID string, current *thread, write func(tx *sql.Tx) error) {
	err := func() error {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}

		if err := write(tx); err != nil {
			tx.Rollback()
			return err
		}

		if _, err := tx.Exec(
			`INSERT INTO chat_threads (desktop_id, running, started_ns) VALUES (?, ?, ?)
			 ON CONFLICT(desktop_id) DO UPDATE SET running = excluded.running, started_ns = excluded.started_ns`,
			desktopID, current.running, sqlitedb.ToNS(current.startedAt),
		); err != nil {
			tx.Rollback()
			return err
		}

		return tx.Commit()
	}()
	if err != nil {
		log.Printf("conversation: saving %s: %v", desktopID, err)
	}
}

func insertEntry(tx *sql.Tx, entry Entry) error {
	_, err := tx.Exec(
		`INSERT INTO chat_entries (id, desktop_id, role, kind, content, task_id, verdict, created_ns)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		entry.ID, entry.DesktopID, entry.Role, entry.Kind, entry.Content,
		entry.TaskID, entry.Verdict, sqlitedb.ToNS(entry.CreatedAt),
	)
	return err
}

func insertStep(tx *sql.Tx, desktopID, entryID string, step Step) error {
	action := ""

	if step.Action != nil {
		encoded, err := json.Marshal(step.Action)
		if err != nil {
			return err
		}
		action = string(encoded)
	}

	_, err := tx.Exec(
		`INSERT INTO chat_steps (id, desktop_id, entry_id, text, action, created_ns)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		step.ID, desktopID, entryID, step.Text, action, sqlitedb.ToNS(step.CreatedAt),
	)
	return err
}

func deleteEntries(tx *sql.Tx, entries []Entry) error {
	for _, entry := range entries {
		if _, err := tx.Exec(`DELETE FROM chat_steps WHERE entry_id = ?`, entry.ID); err != nil {
			return err
		}

		if _, err := tx.Exec(`DELETE FROM chat_entries WHERE id = ?`, entry.ID); err != nil {
			return err
		}
	}

	return nil
}

func deleteSteps(tx *sql.Tx, steps []Step) error {
	for _, step := range steps {
		if _, err := tx.Exec(`DELETE FROM chat_steps WHERE id = ?`, step.ID); err != nil {
			return err
		}
	}

	return nil
}

// load reads one desktop's thread back from the database. Its version starts
// at the current Unix microsecond rather than zero, so a browser still holding
// a version number from before a restart cannot mistake a new snapshot for
// one it already has - and stays below 2^53, the largest integer a browser's
// JSON parser keeps exact. The caller must hold the lock.
func (s *Store) load(desktopID string) *thread {
	loaded := &thread{
		version:     uint64(time.Now().UnixMicro()),
		subscribers: make(map[chan struct{}]bool),
	}

	var running bool
	var started int64

	err := s.db.QueryRow(
		`SELECT running, started_ns FROM chat_threads WHERE desktop_id = ?`, desktopID,
	).Scan(&running, &started)
	if err != nil && err != sql.ErrNoRows {
		log.Printf("conversation: loading %s: %v", desktopID, err)
		return loaded
	}

	loaded.running = running
	loaded.startedAt = sqlitedb.FromNS(started)

	entries, err := loadEntries(s.db, desktopID)
	if err != nil {
		log.Printf("conversation: loading %s: %v", desktopID, err)
		return loaded
	}

	byID := make(map[string]int, len(entries))
	for i, entry := range entries {
		byID[entry.ID] = i
	}

	rows, err := s.db.Query(
		`SELECT id, entry_id, text, action, created_ns FROM chat_steps
		 WHERE desktop_id = ? ORDER BY rowid`, desktopID,
	)
	if err != nil {
		log.Printf("conversation: loading %s: %v", desktopID, err)
		loaded.entries = entries
		return loaded
	}
	defer rows.Close()

	for rows.Next() {
		var step Step
		var entryID, action string
		var created int64

		if err := rows.Scan(&step.ID, &entryID, &step.Text, &action, &created); err != nil {
			log.Printf("conversation: loading %s: %v", desktopID, err)
			continue
		}

		step.CreatedAt = sqlitedb.FromNS(created)

		if action != "" {
			var decoded Action
			if json.Unmarshal([]byte(action), &decoded) == nil {
				step.Action = &decoded
			}
		}

		if entryID == "" {
			loaded.steps = append(loaded.steps, step)
			continue
		}

		if i, ok := byID[entryID]; ok {
			entries[i].Steps = append(entries[i].Steps, step)
		}
	}

	loaded.entries = entries

	return loaded
}

func loadEntries(db *sql.DB, desktopID string) ([]Entry, error) {
	rows, err := db.Query(
		`SELECT id, role, kind, content, task_id, verdict, created_ns FROM chat_entries
		 WHERE desktop_id = ? ORDER BY rowid`, desktopID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []Entry

	for rows.Next() {
		entry := Entry{DesktopID: desktopID}
		var created int64

		if err := rows.Scan(&entry.ID, &entry.Role, &entry.Kind, &entry.Content,
			&entry.TaskID, &entry.Verdict, &created); err != nil {
			return nil, err
		}

		entry.CreatedAt = sqlitedb.FromNS(created)
		entries = append(entries, entry)
	}

	return entries, rows.Err()
}
