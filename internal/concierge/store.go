// Package concierge is the chat-first way into Decurix: one conversation
// with the whole control plane instead of a page per feature. A person types
// what they want; the concierge picks (or is told) a desktop, starts a
// worker session there, and reports back in the same conversation as that
// session runs, asks, or finishes.
//
// A session is one task spun off onto one desktop, numbered (#1, #2, ...)
// so a reply can name it ("#3 yes"). It is a thin pointer: the desktop's own
// agent does the work, the task records (internal/tasks) stay the durable
// history, and the desktop's chat transcript (internal/conversation) is where
// replies are read from - so a session started here and the same desktop's
// own chat page never disagree.
//
// Routing is deterministic before it is clever (the same rule soul's
// atomizer.Triage follows): commands, "#N" replies and "@desktop" targets are
// parsed without a model. A model-backed planner can sit in front of the
// "anything else" branch later without changing what a session is.
//
// The concierge never approves anything on its own. An approval reaches a
// desktop only as the words a person typed, sent through the same request
// path the chat page uses, and the HTTP routes are humansOnly - an agent in a
// desktop (or a prompt injected into one) cannot drive the concierge.
package concierge

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// Message roles.
const (
	RoleUser      = "user"      // what a person typed
	RoleConcierge = "concierge" // the concierge's own words
	RoleAgent     = "agent"     // a worker session's reply, relayed
)

// Session statuses.
const (
	StatusRunning  = "running"
	StatusNeedsYou = "needs_you"
	StatusDone     = "done"
	StatusFailed   = "failed"
	StatusStopped  = "stopped"
	// StatusUnchecked is a job the worker says it finished but whose final check did not judge the result: shown as
	// "finished, not confirmed", never as done (tasks.DisplayOutcome).
	StatusUnchecked = "unchecked"
)

// maxMessages bounds the conversation kept; the oldest go first. The task
// records are the durable history - this is the view.
const maxMessages = 1000

var schema = []string{
	`CREATE TABLE concierge_sessions (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		desktop_id    TEXT NOT NULL,
		desktop_name  TEXT NOT NULL,
		task_id       TEXT NOT NULL,
		request       TEXT NOT NULL,
		status        TEXT NOT NULL,
		kind          TEXT NOT NULL DEFAULT '',
		last_entry_id TEXT NOT NULL DEFAULT '',
		created_ns    INTEGER NOT NULL,
		updated_ns    INTEGER NOT NULL
	)`,
	`CREATE INDEX concierge_sessions_status ON concierge_sessions(status)`,
	`CREATE TABLE concierge_messages (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		role       TEXT NOT NULL,
		session_id INTEGER NOT NULL DEFAULT 0,
		kind       TEXT NOT NULL DEFAULT '',
		text       TEXT NOT NULL,
		created_ns INTEGER NOT NULL
	)`,
}

// Session is one task the concierge spun off onto a desktop.
type Session struct {
	ID          int64     `json:"id"`
	DesktopID   string    `json:"desktop_id"`
	DesktopName string    `json:"desktop_name"`
	TaskID      string    `json:"task_id"`
	Request     string    `json:"request"`
	Status      string    `json:"status"`
	Kind        string    `json:"kind,omitempty"` // the last reply's kind: query/approval/error/entry
	LastEntryID string    `json:"-"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`

	// LiveStep is what the desktop is doing right now; filled in on read,
	// never stored.
	LiveStep string `json:"live_step,omitempty"`

	// ApprovalNote is the open approval this job's Approve/Deny buttons answer - the desktop's newest open question,
	// worded by its approval gate - or "" when there is none and the approval is answered on its card. Filled in on
	// read by the server, never stored.
	ApprovalNote string `json:"approval_note,omitempty"`
}

// Active reports whether the session can still change on its own.
func (s Session) Active() bool {
	return s.Status == StatusRunning || s.Status == StatusNeedsYou
}

// Message is one line of the conversation.
type Message struct {
	ID        int64     `json:"id"`
	Role      string    `json:"role"`
	SessionID int64     `json:"session_id,omitempty"`
	Kind      string    `json:"kind,omitempty"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

// ErrNoSession is returned for a session number that does not exist.
var ErrNoSession = errors.New("no such session")

// Store persists sessions and the conversation in the shared SQLite database.
// Every operation is one statement, so it needs no mutex of its own (the
// querynotes/memory pattern).
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// NewStore migrates the concierge's tables and returns a Store over db.
func NewStore(db *sql.DB) (*Store, error) {
	if err := sqlitedb.Migrate(db, "concierge", schema); err != nil {
		return nil, err
	}
	return &Store{db: db, now: time.Now}, nil
}

// AddMessage appends one message and prunes the oldest past maxMessages.
func (s *Store) AddMessage(role string, sessionID int64, kind, text string) (Message, error) {
	now := s.now()
	res, err := s.db.Exec(
		`INSERT INTO concierge_messages (role, session_id, kind, text, created_ns) VALUES (?, ?, ?, ?, ?)`,
		role, sessionID, kind, text, now.UnixNano(),
	)
	if err != nil {
		return Message{}, fmt.Errorf("add concierge message: %w", err)
	}
	id, _ := res.LastInsertId()
	if _, err := s.db.Exec(
		`DELETE FROM concierge_messages WHERE id <= ?`, id-maxMessages,
	); err != nil {
		return Message{}, fmt.Errorf("prune concierge messages: %w", err)
	}
	return Message{ID: id, Role: role, SessionID: sessionID, Kind: kind, Text: text, CreatedAt: now}, nil
}

// Messages returns messages with an ID above after, oldest first, at most limit.
func (s *Store) Messages(after int64, limit int) ([]Message, error) {
	rows, err := s.db.Query(
		`SELECT id, role, session_id, kind, text, created_ns FROM (
			SELECT * FROM concierge_messages WHERE id > ? ORDER BY id DESC LIMIT ?
		) ORDER BY id ASC`, after, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list concierge messages: %w", err)
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		var m Message
		var ns int64
		if err := rows.Scan(&m.ID, &m.Role, &m.SessionID, &m.Kind, &m.Text, &ns); err != nil {
			return nil, err
		}
		m.CreatedAt = time.Unix(0, ns)
		out = append(out, m)
	}
	return out, rows.Err()
}

// CreateSession records a new running session and returns it with its number.
func (s *Store) CreateSession(desktopID, desktopName, taskID, request string) (Session, error) {
	now := s.now()
	res, err := s.db.Exec(
		`INSERT INTO concierge_sessions (desktop_id, desktop_name, task_id, request, status, created_ns, updated_ns)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		desktopID, desktopName, taskID, request, StatusRunning, now.UnixNano(), now.UnixNano(),
	)
	if err != nil {
		return Session{}, fmt.Errorf("create concierge session: %w", err)
	}
	id, _ := res.LastInsertId()
	return Session{
		ID: id, DesktopID: desktopID, DesktopName: desktopName, TaskID: taskID,
		Request: request, Status: StatusRunning, CreatedAt: now, UpdatedAt: now,
	}, nil
}

// Session returns session id, or ErrNoSession.
func (s *Store) Session(id int64) (Session, error) {
	list, err := s.querySessions(`WHERE id = ?`, id)
	if err != nil {
		return Session{}, err
	}
	if len(list) == 0 {
		return Session{}, ErrNoSession
	}
	return list[0], nil
}

// ActiveSessions returns every running or waiting session, oldest first.
func (s *Store) ActiveSessions() ([]Session, error) {
	return s.querySessions(`WHERE status IN (?, ?) ORDER BY id ASC`, StatusRunning, StatusNeedsYou)
}

// RecentSessions returns the newest limit sessions, newest first.
func (s *Store) RecentSessions(limit int) ([]Session, error) {
	return s.querySessions(`ORDER BY id DESC LIMIT ?`, limit)
}

// UpdateSession sets a session's status, last reply kind and the transcript
// entry it has relayed up to.
func (s *Store) UpdateSession(id int64, status, kind, lastEntryID string) error {
	_, err := s.db.Exec(
		`UPDATE concierge_sessions SET status = ?, kind = ?, last_entry_id = ?, updated_ns = ? WHERE id = ?`,
		status, kind, lastEntryID, s.now().UnixNano(), id,
	)
	if err != nil {
		return fmt.Errorf("update concierge session: %w", err)
	}
	return nil
}

func (s *Store) querySessions(where string, args ...any) ([]Session, error) {
	rows, err := s.db.Query(
		`SELECT id, desktop_id, desktop_name, task_id, request, status, kind, last_entry_id, created_ns, updated_ns
		 FROM concierge_sessions `+where, args...,
	)
	if err != nil {
		return nil, fmt.Errorf("list concierge sessions: %w", err)
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		var ss Session
		var created, updated int64
		if err := rows.Scan(&ss.ID, &ss.DesktopID, &ss.DesktopName, &ss.TaskID, &ss.Request,
			&ss.Status, &ss.Kind, &ss.LastEntryID, &created, &updated); err != nil {
			return nil, err
		}
		ss.CreatedAt = time.Unix(0, created)
		ss.UpdatedAt = time.Unix(0, updated)
		out = append(out, ss)
	}
	return out, rows.Err()
}
