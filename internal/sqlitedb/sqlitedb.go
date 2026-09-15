// Package sqlitedb opens the control plane's one embedded SQLite database and
// applies each component's schema to it.
//
// The driver is modernc.org/sqlite, a pure-Go translation of SQLite, so the
// release builds stay CGO_ENABLED=0 static binaries for every target. Every
// store that must survive a restart (conversation, querynotes, memory,
// stepimages, tasks) is handed the same *sql.DB and owns its own tables.
package sqlitedb

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// FileName is the database file inside the data directory.
const FileName = "flowera.db"

// DataDirEnv names the directory the database lives in.
const DataDirEnv = "FLOWERA_DATA_DIR"

// DataDir returns where the control plane keeps its records: FLOWERA_DATA_DIR
// when set, otherwise a "flowera" directory under the user's config directory,
// so a downloaded binary finds the same records whichever directory it is
// started from. The Docker image sets FLOWERA_DATA_DIR to its /data volume.
func DataDir() string {
	if dir := strings.TrimSpace(os.Getenv(DataDirEnv)); dir != "" {
		return dir
	}

	if base, err := os.UserConfigDir(); err == nil {
		return filepath.Join(base, "flowera")
	}

	return "data"
}

// Open opens (creating if needed) the database file at path.
//
// The pool is held to one connection. Writes to SQLite serialise anyway, and a
// single connection rules out SQLITE_BUSY between a reader upgrading to a
// writer and another writer - the price is that no caller may run a query
// while it still holds an open *sql.Rows, which every store here respects by
// reading rows to the end before doing anything else.
func Open(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("sqlitedb: creating %s: %w", dir, err)
		}
	}

	query := url.Values{}
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "journal_mode(WAL)")
	query.Add("_pragma", "synchronous(NORMAL)")
	query.Add("_pragma", "foreign_keys(ON)")

	return open("file:" + filepath.ToSlash(path) + "?" + query.Encode())
}

// OpenMemory opens a private in-memory database, for tests. With the pool held
// to one connection, every query sees the same database.
func OpenMemory() (*sql.DB, error) {
	return open("file::memory:?_pragma=foreign_keys(ON)")
}

func open(dsn string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlitedb: open: %w", err)
	}

	db.SetMaxOpenConns(1)
	db.SetConnMaxIdleTime(0)
	db.SetConnMaxLifetime(0)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlitedb: open: %w", err)
	}

	return db, nil
}

// Migrate applies the schema steps a component has not applied yet, in order,
// each in its own transaction. A component's steps are append-only: an applied
// step is never edited, a change is a new step at the end.
func Migrate(db *sql.DB, component string, steps []string) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		component TEXT PRIMARY KEY,
		version   INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("sqlitedb: migrations table: %w", err)
	}

	var applied int

	err := db.QueryRow(
		`SELECT version FROM schema_migrations WHERE component = ?`,
		component,
	).Scan(&applied)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("sqlitedb: reading %s schema version: %w", component, err)
	}

	for version := applied; version < len(steps); version++ {
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("sqlitedb: %s step %d: %w", component, version+1, err)
		}

		if _, err := tx.Exec(steps[version]); err != nil {
			tx.Rollback()
			return fmt.Errorf("sqlitedb: %s step %d: %w", component, version+1, err)
		}

		if _, err := tx.Exec(
			`INSERT INTO schema_migrations (component, version) VALUES (?, ?)
			 ON CONFLICT(component) DO UPDATE SET version = excluded.version`,
			component, version+1,
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("sqlitedb: %s step %d: %w", component, version+1, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("sqlitedb: %s step %d: %w", component, version+1, err)
		}
	}

	return nil
}

// ToNS stores a time as Unix nanoseconds; the zero time is stored as 0.
func ToNS(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}

	return t.UnixNano()
}

// FromNS is ToNS's inverse, in UTC.
func FromNS(ns int64) time.Time {
	if ns == 0 {
		return time.Time{}
	}

	return time.Unix(0, ns).UTC()
}
