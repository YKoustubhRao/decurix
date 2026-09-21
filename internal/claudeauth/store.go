// Package claudeauth holds the one shared Claude Code login every
// claude-harness desktop is seeded from at creation time, in place of the
// ANTHROPIC_API_KEY that used to be copied into a new desktop's environment.
// The session lives in the control plane's SQLite database, so it survives a
// restart the same way task records and chats do, build.sh leaves it alone
// the same way it leaves decurix-data alone, and rebuild.sh wipes it for free
// as part of wiping that same volume - no script changes needed for either
// half of that.
package claudeauth

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// Session is the shared Claude Code login: the raw ~/.claude/.credentials.json
// content every new claude-harness desktop is seeded with, and who it belongs
// to for display in the dashboard.
type Session struct {
	CredentialsJSON string    `json:"-"`
	Email           string    `json:"email"`
	UpdatedAt       time.Time `json:"updated_at"`
}

var schema = []string{
	`CREATE TABLE claude_session (
		id               INTEGER PRIMARY KEY CHECK (id = 1),
		credentials_json TEXT NOT NULL,
		email            TEXT NOT NULL DEFAULT '',
		updated_ns       INTEGER NOT NULL
	)`,
}

// Store is the single-row SQLite-backed home of the shared Claude session.
type Store struct {
	db *sql.DB
}

// NewStore opens the claudeauth Store on db, creating its table if needed.
func NewStore(db *sql.DB) (*Store, error) {
	if err := sqlitedb.Migrate(db, "claudeauth", schema); err != nil {
		return nil, err
	}

	return &Store{db: db}, nil
}

// Get returns the currently stored shared Claude session, or ok=false if no
// one has logged in yet (or a rebuild wiped the database since).
func (s *Store) Get() (session Session, ok bool) {
	var updated int64

	err := s.db.QueryRow(
		`SELECT credentials_json, email, updated_ns FROM claude_session WHERE id = 1`,
	).Scan(&session.CredentialsJSON, &session.Email, &updated)
	if err != nil {
		return Session{}, false
	}

	session.UpdatedAt = sqlitedb.FromNS(updated)

	return session, true
}

// Save replaces the stored shared Claude session - the one row every
// claude-harness desktop is seeded from at creation, from then until the
// next login overwrites it or a rebuild clears the database.
func (s *Store) Save(credentialsJSON, email string) error {
	_, err := s.db.Exec(
		`INSERT INTO claude_session (id, credentials_json, email, updated_ns)
		 VALUES (1, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
			credentials_json = excluded.credentials_json,
			email            = excluded.email,
			updated_ns       = excluded.updated_ns`,
		credentialsJSON, email, sqlitedb.ToNS(time.Now()),
	)
	if err != nil {
		return fmt.Errorf("claudeauth: save: %w", err)
	}

	return nil
}
