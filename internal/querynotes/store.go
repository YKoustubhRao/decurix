// Package querynotes holds the record of queries desktop agents raise for a
// human, kept in the control plane's SQLite database so a restart does not
// empty the Query rail.
package querynotes

import (
	"database/sql"
	"log"
	"time"

	"github.com/YKoustubhRao/flowera/internal/sqlitedb"
)

// QueryNote is a support note submitted from inside a running desktop, tied to a specific desktop and VM.
type QueryNote struct {
	ID        string    `json:"id"`
	DesktopID string    `json:"desktop_id"`
	VMName    string    `json:"vm_name"`
	Filename  string    `json:"filename"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

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
}

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
		`INSERT INTO query_notes (id, desktop_id, vm_name, filename, content, created_ns)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET content = excluded.content`,
		note.ID, note.DesktopID, note.VMName, note.Filename, note.Content,
		sqlitedb.ToNS(note.CreatedAt),
	)
	if err != nil {
		log.Printf("querynotes: add %s: %v", note.ID, err)
	}
}

// List returns all recorded query notes, oldest first.
func (s *Store) List() []QueryNote {
	rows, err := s.db.Query(
		`SELECT id, desktop_id, vm_name, filename, content, created_ns
		 FROM query_notes ORDER BY created_ns`,
	)
	if err != nil {
		log.Printf("querynotes: list: %v", err)
		return []QueryNote{}
	}
	defer rows.Close()

	notes := make([]QueryNote, 0)

	for rows.Next() {
		var note QueryNote
		var created int64

		if err := rows.Scan(&note.ID, &note.DesktopID, &note.VMName, &note.Filename, &note.Content, &created); err != nil {
			log.Printf("querynotes: list: %v", err)
			continue
		}

		note.CreatedAt = sqlitedb.FromNS(created)
		notes = append(notes, note)
	}

	return notes
}

// Close deletes a query note by ID. It is a no-op if the ID is not present.
// The request itself is not lost with it: the task record keeps every query
// and the reply it got (internal/tasks).
func (s *Store) Close(id string) {
	if _, err := s.db.Exec(`DELETE FROM query_notes WHERE id = ?`, id); err != nil {
		log.Printf("querynotes: close %s: %v", id, err)
	}
}
