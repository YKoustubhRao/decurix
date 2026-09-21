// Package querynotes holds the record of queries desktop agents raise for a
// human, kept in the control plane's SQLite database so a restart does not
// empty the Query rail.
package querynotes

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// QueryNote is a support note submitted from inside a running desktop, tied to a specific desktop and VM.
type QueryNote struct {
	ID        string    `json:"id"`
	DesktopID string    `json:"desktop_id"`
	VMName    string    `json:"vm_name"`
	Filename  string    `json:"filename"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`

	// Kind is what the desktop was doing when it wrote the note: "query" or
	// "approval" (it is waiting on a human), "error" or "done". Empty on a note
	// from before it was recorded.
	Kind string `json:"kind,omitempty"`

	// ResolvedAt and Resolution are set once the note has been dealt with, and
	// only then: Resolution is ResolutionAnswered when a reply in the chat (or
	// anywhere else a request reaches the desktop) answered it, and
	// ResolutionManual when a person marked it resolved.
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	Resolution string     `json:"resolution,omitempty"`
}

// Resolutions a note can end with.
const (
	ResolutionAnswered = "answered"
	ResolutionManual   = "manual"
)

// Kinds of note that mean the desktop is waiting on a human, which is exactly
// the ones a reply answers.
const (
	KindQuery    = "query"
	KindApproval = "approval"
)

var schema = []string{
	`CREATE TABLE help_notes (
		id         TEXT PRIMARY KEY,
		desktop_id TEXT NOT NULL,
		vm_name    TEXT NOT NULL,
		filename   TEXT NOT NULL,
		content    TEXT NOT NULL,
		created_ns INTEGER NOT NULL
	)`,
	// The step above is applied on every existing installation and must never
	// be edited - schema changes only ever append.
	`ALTER TABLE help_notes RENAME TO query_notes`,
	// A resolved note is kept, not deleted: the Queries page's Resolved
	// section is the history. resolved_ns 0 is "still open".
	`ALTER TABLE query_notes ADD COLUMN kind TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE query_notes ADD COLUMN resolved_ns INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE query_notes ADD COLUMN resolution TEXT NOT NULL DEFAULT ''`,
}

// maxResolved bounds one ListResolved call, so the history stays flat-cost as
// it grows.
const maxResolved = 200

// Store is a SQLite-backed collection of QueryNotes.
type Store struct {
	db *sql.DB
}

// NewStore opens the query-notes Store on db, creating its table if needed.
func NewStore(db *sql.DB) (*Store, error) {
	if err := sqlitedb.Migrate(db, "helpnotes", schema); err != nil {
		return nil, err
	}

	return &Store{db: db}, nil
}

// Add records a query note. A note that cannot be written is logged rather
// than failing the desktop's callback: the same callback also carries the
// chat reply and the task outcome, which must still land.
func (s *Store) Add(note QueryNote) {
	_, err := s.db.Exec(
		`INSERT INTO query_notes (id, desktop_id, vm_name, filename, content, created_ns, kind)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET content = excluded.content`,
		note.ID, note.DesktopID, note.VMName, note.Filename, note.Content,
		sqlitedb.ToNS(note.CreatedAt), note.Kind,
	)
	if err != nil {
		log.Printf("querynotes: add %s: %v", note.ID, err)
	}
}

const noteColumns = `id, desktop_id, vm_name, filename, content, created_ns, kind, resolved_ns, resolution`

// list reads the notes a WHERE clause selects, in the order given, closing its
// rows before returning (no query may run while a *sql.Rows is open).
func (s *Store) list(where, order string, args ...any) []QueryNote {
	rows, err := s.db.Query(
		`SELECT `+noteColumns+` FROM query_notes WHERE `+where+` ORDER BY `+order, args...,
	)
	if err != nil {
		log.Printf("querynotes: list: %v", err)
		return []QueryNote{}
	}
	defer rows.Close()

	notes := make([]QueryNote, 0)

	for rows.Next() {
		var note QueryNote
		var created, resolved int64

		if err := rows.Scan(&note.ID, &note.DesktopID, &note.VMName, &note.Filename, &note.Content,
			&created, &note.Kind, &resolved, &note.Resolution); err != nil {
			log.Printf("querynotes: list: %v", err)
			continue
		}

		note.CreatedAt = sqlitedb.FromNS(created)

		if resolved != 0 {
			at := sqlitedb.FromNS(resolved)
			note.ResolvedAt = &at
		}

		notes = append(notes, note)
	}

	return notes
}

// List returns the notes still open, oldest first.
func (s *Store) List() []QueryNote {
	return s.list(`resolved_ns = 0`, `created_ns`)
}

// ListResolved returns the notes that have been resolved, most recently
// resolved first, up to maxResolved.
func (s *Store) ListResolved() []QueryNote {
	notes := s.list(`resolved_ns != 0`, `resolved_ns DESC LIMIT `+fmt.Sprint(maxResolved))
	return notes
}

// Close resolves a query note by ID, keeping it in the history. It is a no-op
// if the ID is not present or the note is already resolved. The request itself
// is not lost either way: the task record keeps every query and the reply it
// got (internal/tasks).
func (s *Store) Close(id string) {
	s.resolve(`id = ?`, ResolutionManual, id)
}

// ResolveAnswered resolves every open note on a desktop that was waiting on a
// human (a query or an approval), because a request has just reached that
// desktop as the reply. A note of any other kind - a finished task's report -
// is not something a reply answers, and stays open until someone resolves it.
func (s *Store) ResolveAnswered(desktopID string) {
	s.resolve(`desktop_id = ? AND kind IN (?, ?)`, ResolutionAnswered, desktopID, KindQuery, KindApproval)
}

func (s *Store) resolve(where, resolution string, args ...any) {
	all := append([]any{sqlitedb.ToNS(time.Now()), resolution}, args...)

	if _, err := s.db.Exec(
		`UPDATE query_notes SET resolved_ns = ?, resolution = ? WHERE resolved_ns = 0 AND `+where, all...,
	); err != nil {
		log.Printf("querynotes: resolve (%s): %v", resolution, err)
	}
}
