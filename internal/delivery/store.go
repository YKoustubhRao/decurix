// Package delivery keeps what crosses between the control plane and a desktop durable, so a restart of either side
// neither loses a message nor applies one twice.
//
// Two records, both in the control plane's SQLite database:
//
//   - Sending: a request the control plane has started to send a desktop and not yet recorded in the task records.
//     It is written before delivery and removed once the records hold the request. A row still here when the
//     process starts is a send a crash cut short: the desktop may or may not have it (httpapi's
//     ReconcileDeliveries settles each one). The row keeps the request only in the form the records would keep it
//     (withheld), never as typed.
//   - Applied events: the ids of messages a desktop sent (a question, an approval, an outcome) that were applied.
//     A desktop resends a message until it is acknowledged; the same id again is acknowledged without being applied
//     twice.
//
// The technique is borrowed from agent-orchestrator's detached chat hosts: every crossing carries a stable id, and
// is acknowledged only after it is committed.
package delivery

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// States of a sending row.
const (
	// StateSending is a request on its way to a desktop.
	StateSending = "sending"
	// StateUnconfirmed is a request a crash cut short whose desktop could not say whether it has it. Its desktop's
	// late outcome for that task is accepted (TakeUnconfirmed) rather than refused as a task nobody opened.
	StateUnconfirmed = "unconfirmed"
)

// eventKeep is how long an applied event's id is remembered. A desktop resends an undelivered message for a day.
const eventKeep = 72 * time.Hour

// unconfirmedKeep is how long an unconfirmed send waits for its desktop's late outcome.
const unconfirmedKeep = 7 * 24 * time.Hour

var schema = []string{
	`CREATE TABLE sending (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		desktop_id  TEXT NOT NULL,
		task_id     TEXT NOT NULL,
		state       TEXT NOT NULL,
		payload     BLOB NOT NULL,
		started_ns  INTEGER NOT NULL,
		settled_ns  INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX sending_desktop_task ON sending (desktop_id, task_id)`,
	`CREATE TABLE applied_events (
		desktop_id  TEXT NOT NULL,
		event_id    TEXT NOT NULL,
		applied_ns  INTEGER NOT NULL,
		PRIMARY KEY (desktop_id, event_id)
	)`,
	`CREATE INDEX applied_events_at ON applied_events (applied_ns)`,
}

// ErrNotFound is returned for a sending row that is not there.
var ErrNotFound = errors.New("delivery: not found")

// Sending is a request on its way to a desktop, or one a crash cut short.
type Sending struct {
	ID        int64
	DesktopID string
	TaskID    string
	State     string
	// Payload is the caller's own record of the send (httpapi keeps the route, the worker, who sent it and the
	// request as the records would keep it), opaque here.
	Payload   []byte
	StartedAt time.Time
}

// Store is the delivery records.
type Store struct {
	db  *sql.DB
	now func() time.Time

	// inflight holds the events being applied right now, so two copies of one message arriving together are applied
	// once: the second waits for the first's outcome.
	mu       sync.Mutex
	inflight map[string]chan struct{}
}

// NewStore opens the delivery records on db, creating its tables if needed.
func NewStore(db *sql.DB) (*Store, error) {
	if err := sqlitedb.Migrate(db, "delivery", schema); err != nil {
		return nil, err
	}
	return &Store{db: db, now: time.Now, inflight: map[string]chan struct{}{}}, nil
}

// Begin records that a request for taskID is about to be sent to desktopID, before anything is sent, and returns
// the row's id for Done.
func (s *Store) Begin(desktopID, taskID string, payload []byte) (int64, error) {
	if payload == nil {
		payload = []byte{}
	}
	result, err := s.db.Exec(`INSERT INTO sending (desktop_id, task_id, state, payload, started_ns) VALUES (?, ?, ?, ?, ?)`,
		desktopID, taskID, StateSending, payload, sqlitedb.ToNS(s.now()))
	if err != nil {
		return 0, fmt.Errorf("delivery: begin: %w", err)
	}
	return result.LastInsertId()
}

// Done removes a sending row: the request is in the task records, or it was never delivered.
func (s *Store) Done(id int64) error {
	if _, err := s.db.Exec(`DELETE FROM sending WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delivery: done %d: %w", id, err)
	}
	return nil
}

// Leftovers is every send that began before since and is still marked sending: what a crash cut short. A send this
// process began is never one, since `since` is when it started.
func (s *Store) Leftovers(since time.Time) ([]Sending, error) {
	return s.list(`state = ? AND started_ns < ?`, StateSending, sqlitedb.ToNS(since))
}

// MarkUnconfirmed keeps a cut-short send whose desktop could not say whether it has it, so that desktop's late
// outcome for its task is accepted.
func (s *Store) MarkUnconfirmed(id int64) error {
	result, err := s.db.Exec(`UPDATE sending SET state = ?, settled_ns = ? WHERE id = ?`,
		StateUnconfirmed, sqlitedb.ToNS(s.now()), id)
	if err != nil {
		return fmt.Errorf("delivery: unconfirmed %d: %w", id, err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// TakeUnconfirmed removes and returns the unconfirmed send of taskID to desktopID, if there is one: its desktop has
// now answered it, so it did arrive. ok is false when there is none.
func (s *Store) TakeUnconfirmed(desktopID, taskID string) (Sending, bool, error) {
	if desktopID == "" || taskID == "" {
		return Sending{}, false, nil
	}
	found, err := s.list(`state = ? AND desktop_id = ? AND task_id = ?`, StateUnconfirmed, desktopID, taskID)
	if err != nil || len(found) == 0 {
		return Sending{}, false, err
	}
	taken := found[len(found)-1]
	result, err := s.db.Exec(`DELETE FROM sending WHERE id = ? AND state = ?`, taken.ID, StateUnconfirmed)
	if err != nil {
		return Sending{}, false, fmt.Errorf("delivery: take %d: %w", taken.ID, err)
	}
	// Taken by another caller between the read and the delete: it is theirs.
	if n, _ := result.RowsAffected(); n == 0 {
		return Sending{}, false, nil
	}
	return taken, true, nil
}

// list reads the sending rows a WHERE clause selects, oldest first, closing its rows before returning.
func (s *Store) list(where string, args ...any) ([]Sending, error) {
	found, err := sqlitedb.QueryAll(s.db,
		func(row sqlitedb.Scanner) (Sending, error) {
			var item Sending
			var started int64
			err := row.Scan(&item.ID, &item.DesktopID, &item.TaskID, &item.State, &item.Payload, &started)
			item.StartedAt = sqlitedb.FromNS(started)
			return item, err
		},
		`SELECT id, desktop_id, task_id, state, payload, started_ns FROM sending WHERE `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("delivery: list: %w", err)
	}
	return found, nil
}

// Apply runs apply for a desktop's message unless an event with this id from this desktop was already applied, and
// remembers the id only once apply has returned true (the message was applied, not refused): acknowledged after it is
// committed. It reports whether the message was a duplicate (apply not run). An empty eventID (a desktop built before
// messages carried one) is always applied.
//
// Two copies arriving together are applied once: the second waits for the first, then reads as a duplicate.
func (s *Store) Apply(desktopID, eventID string, apply func() bool) (duplicate bool, err error) {
	if eventID == "" {
		apply()
		return false, nil
	}
	key := desktopID + "\x00" + eventID
	for {
		s.mu.Lock()
		wait, busy := s.inflight[key]
		if !busy {
			done := make(chan struct{})
			s.inflight[key] = done
			s.mu.Unlock()
			defer func() {
				s.mu.Lock()
				delete(s.inflight, key)
				s.mu.Unlock()
				close(done)
			}()
			break
		}
		s.mu.Unlock()
		<-wait
	}

	var seen int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM applied_events WHERE desktop_id = ? AND event_id = ?`,
		desktopID, eventID).Scan(&seen); err != nil {
		// Unreadable: applying it is the safer mistake (a message shown twice, never one lost).
		apply()
		return false, fmt.Errorf("delivery: reading event %s: %w", eventID, err)
	}
	if seen > 0 {
		return true, nil
	}
	if !apply() {
		return false, nil
	}
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO applied_events (desktop_id, event_id, applied_ns) VALUES (?, ?, ?)`,
		desktopID, eventID, sqlitedb.ToNS(s.now())); err != nil {
		return false, fmt.Errorf("delivery: remembering event %s: %w", eventID, err)
	}
	return false, nil
}

// Prune forgets applied events past their keep and unconfirmed sends nobody answered within theirs.
func (s *Store) Prune() error {
	now := s.now()
	if _, err := s.db.Exec(`DELETE FROM applied_events WHERE applied_ns < ?`, sqlitedb.ToNS(now.Add(-eventKeep))); err != nil {
		return fmt.Errorf("delivery: prune events: %w", err)
	}
	if _, err := s.db.Exec(`DELETE FROM sending WHERE state = ? AND settled_ns < ?`,
		StateUnconfirmed, sqlitedb.ToNS(now.Add(-unconfirmedKeep))); err != nil {
		return fmt.Errorf("delivery: prune sends: %w", err)
	}
	return nil
}
