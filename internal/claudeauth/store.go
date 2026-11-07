// Package claudeauth holds the one shared Claude Code login every
// claude-harness desktop is seeded from at creation time, in place of the
// ANTHROPIC_API_KEY that used to be copied into a new desktop's environment.
// The session lives in the control plane's SQLite database, so it survives a
// restart the same way task records and chats do, build.sh leaves it alone
// the same way it leaves decurix-data alone, and rebuild.sh wipes it for free
// as part of wiping that same volume - no script changes needed for either
// half of that.
//
// The login holds a refresh token, so it is sealed (internal/sealed) like the
// other secrets the control plane keeps, under its own key file (KeyFile) once
// UseKey is called. A row saved by an older build, before it was sealed, is
// sealed in place the first time the key is given (UseKey) or the row is read.
package claudeauth

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sealed"
	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// KeyFile is the data-dir file sealing the shared Claude login (sealed.LoadKey).
const KeyFile = "claude-session.key"

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
	db  *sql.DB
	mu  sync.Mutex // a read that reseals an old row reads, then writes
	key []byte     // nil: stored as is (tests, a data directory that could not be written)
}

// NewStore opens the claudeauth Store on db, creating its table if needed.
func NewStore(db *sql.DB) (*Store, error) {
	if err := sqlitedb.Migrate(db, "claudeauth", schema); err != nil {
		return nil, err
	}

	return &Store{db: db}, nil
}

// UseKey seals the session with key from now on, and seals in place a row an
// older build saved unsealed. A nil key changes nothing.
func (s *Store) UseKey(key []byte) error {
	if key == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.key = key

	return s.resealLocked()
}

// resealLocked seals the stored credentials when they were saved unsealed; s.mu is held.
func (s *Store) resealLocked() error {
	if s.key == nil {
		return nil
	}
	var stored string
	err := s.db.QueryRow(`SELECT credentials_json FROM claude_session WHERE id = 1`).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && sealed.IsSealed(stored)) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("claudeauth: read for sealing: %w", err)
	}
	sealedJSON, err := sealed.Seal(s.key, stored)
	if err != nil {
		return fmt.Errorf("claudeauth: seal: %w", err)
	}
	if _, err := s.db.Exec(`UPDATE claude_session SET credentials_json = ? WHERE id = 1 AND credentials_json = ?`,
		sealedJSON, stored); err != nil {
		return fmt.Errorf("claudeauth: seal in place: %w", err)
	}

	return nil
}

// Get returns the currently stored shared Claude session, or ok=false if no
// one has logged in yet (or a rebuild wiped the database since), or the
// stored login cannot be opened with this key (the key file was replaced:
// only logging in again fixes it).
func (s *Store) Get() (session Session, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var updated int64
	var stored string

	err := s.db.QueryRow(
		`SELECT credentials_json, email, updated_ns FROM claude_session WHERE id = 1`,
	).Scan(&stored, &session.Email, &updated)
	if err != nil {
		return Session{}, false
	}

	plain, err := sealed.Open(s.key, stored)
	if err != nil {
		log.Printf("claudeauth: the stored Claude login cannot be opened (%v); log in again", err)
		return Session{}, false
	}
	if !sealed.IsSealed(stored) && s.key != nil {
		if err := s.resealLocked(); err != nil {
			log.Printf("claudeauth: %v", err)
		}
	}

	session.CredentialsJSON = plain
	session.UpdatedAt = sqlitedb.FromNS(updated)

	return session, true
}

// Save replaces the stored shared Claude session - the one row every
// claude-harness desktop is seeded from at creation, from then until the
// next login overwrites it or a rebuild clears the database. The credentials
// are sealed when the store has a key.
func (s *Store) Save(credentialsJSON, email string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, err := sealed.Seal(s.key, credentialsJSON)
	if err != nil {
		return fmt.Errorf("claudeauth: seal: %w", err)
	}

	_, err = s.db.Exec(
		`INSERT INTO claude_session (id, credentials_json, email, updated_ns)
		 VALUES (1, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
			credentials_json = excluded.credentials_json,
			email            = excluded.email,
			updated_ns       = excluded.updated_ns`,
		stored, email, sqlitedb.ToNS(time.Now()),
	)
	if err != nil {
		return fmt.Errorf("claudeauth: save: %w", err)
	}

	return nil
}
