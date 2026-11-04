// Package sqlitedb opens the control plane's one embedded SQLite database and
// applies each component's schema to it.
//
// The driver is modernc.org/sqlite, a pure-Go translation of SQLite, so the
// release builds stay CGO_ENABLED=0 static binaries for every target. Every
// store that must survive a restart (conversation, querynotes, memory,
// stepimages, tasks) is handed the same *sql.DB and owns its own tables.
package sqlitedb

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// FileName is the database file inside the data directory.
const FileName = "decurix.db"

// DataDirEnv names the directory the database lives in.
const DataDirEnv = "DECURIX_DATA_DIR"

// DataDir returns where the control plane keeps its records: DECURIX_DATA_DIR
// when set, otherwise a "decurix" directory under the user's config directory,
// so a downloaded binary finds the same records whichever directory it is
// started from. The Docker image sets DECURIX_DATA_DIR to its /data volume.
func DataDir() string {
	if dir := strings.TrimSpace(os.Getenv(DataDirEnv)); dir != "" {
		return dir
	}

	if base, err := os.UserConfigDir(); err == nil {
		return filepath.Join(base, "decurix")
	}

	return "data"
}

// maxOpenConns bounds the connection pool. WAL journal mode (below) lets any
// number of readers run alongside the one connection actually writing at a
// given instant, so the pool no longer needs to be held to one connection to
// stay correct - busy_timeout absorbs the rare case of two connections
// writing at once, the same as it always has. Sized like this codebase's
// other fixed concurrency limits (desktop.listInspectConcurrency,
// idlepause.sweepConcurrency): enough that a burst of readers doesn't queue
// behind each other, small enough to stay a bounded resource on a
// CPU/RAM-constrained host. A store that reads a row and writes it back
// still needs its own mutex for that read-modify-write to stay atomic now
// that "one caller on the connection at a time" no longer provides it for
// free - see schedule.Store.mu for the one store that needed it.
const maxOpenConns = 8

// Open opens (creating if needed) the database file at path.
//
// No caller may run a query while it still holds an open *sql.Rows: an idle
// *sql.Rows on one connection can't be reasoned about by a second query
// issued from the same goroutine (nothing frees the first connection back to
// the pool until its Rows is closed), and a long-open read transaction holds
// back WAL's checkpoint. Every store here reads rows to the end, and closes
// them, before its next statement - keep it that way.
func Open(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("sqlitedb: creating %s: %w", dir, err)
		}
	}

	query := url.Values{}
	query.Add("_pragma", "busy_timeout(10000)")
	query.Add("_pragma", "journal_mode(WAL)")
	query.Add("_pragma", "synchronous(NORMAL)")
	query.Add("_pragma", "foreign_keys(ON)")
	query.Add("_pragma", "auto_vacuum(INCREMENTAL)")
	query.Add("_pragma", "temp_store(MEMORY)")
	// 4 MB of page cache a connection, eight connections: 32 MB at most, where 16 MB each held up to 128 MB for a
	// database the OS file cache and the mmap below already serve (resource plan P1 #6).
	query.Add("_pragma", "cache_size(-4000)")
	query.Add("_pragma", "mmap_size(67108864)")
	// Every transaction takes the write lock when it begins (BEGIN IMMEDIATE),
	// waiting under busy_timeout. A deferred one that read first and wrote
	// after another connection committed failed at once with
	// SQLITE_BUSY_SNAPSHOT (517, "database is locked"), which no timeout
	// retries: a finished task's outcome was lost that way. Stores open
	// transactions only to write, so none waits for nothing.
	query.Add("_txlock", "immediate")

	// Escape URI metacharacters in directory and file names; otherwise a '?'
	// silently becomes driver options and opens a different database.
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("sqlitedb: resolving path: %w", err)
	}
	uriPath := filepath.ToSlash(absPath)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath // Windows drive letter in a file URI.
	}
	dsn := url.URL{Scheme: "file", Path: uriPath, RawQuery: query.Encode()}
	db, err := open(dsn.String(), maxOpenConns)
	if err != nil {
		return nil, err
	}
	if _, err := ensureIncrementalVacuum(db, absPath); err != nil {
		log.Printf("sqlitedb: %s: could not turn on incremental vacuum: %v", absPath, err)
	}
	return db, nil
}

// ensureIncrementalVacuum turns on incremental vacuum for a database made before it was asked for: the
// auto_vacuum(INCREMENTAL) above takes effect only on a database that has no tables yet, so on an existing one it
// did nothing and freed pages were never given back (resource plan B2). Only when the setting differs: it is set, and
// one VACUUM rebuilds the file so it holds. It reports whether it did.
//
// The VACUUM runs on a connection of its own with temp_store = FILE: with the pool's temp_store = MEMORY its copy of
// the database was built in RAM (measured on a 58 MB database: 231 MB at the peak, against 122 MB in a file; a 1 GB
// database would have spiked 3-4 GB at start). WAL mode writes the rebuilt file through the WAL, so a TRUNCATE
// checkpoint after it gives that back (it stayed the database's size for the life of the process). It is skipped,
// and said why, when the data folder or the temp folder has less than twice the database free; it is tried again at
// the next start. Size before and after, and the time it took, are logged.
func ensureIncrementalVacuum(db *sql.DB, path string) (bool, error) {
	var mode int
	if err := db.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		return false, err
	}
	if mode == 2 { // INCREMENTAL
		return false, nil
	}
	size, err := databaseSize(db)
	if err != nil {
		return false, err
	}
	need := 2 * size
	for _, dir := range []string{filepath.Dir(path), os.TempDir()} {
		if free, err := diskFree(dir); err == nil && free < uint64(need) {
			log.Printf("sqlitedb: %s: not turning on incremental vacuum yet: it needs %d MB free in %s for one VACUUM, and %d MB is; it is tried again at the next start",
				path, need>>20, dir, free>>20)
			return false, nil
		}
	}
	log.Printf("sqlitedb: %s: turning on incremental vacuum (auto_vacuum was %d); one VACUUM of %d MB", path, mode, size>>20)
	started := time.Now()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA temp_store = FILE`); err != nil {
		return false, err
	}
	// The pool's own setting, on this connection too, whatever happens below.
	defer func() { _, _ = conn.ExecContext(ctx, `PRAGMA temp_store = MEMORY`) }()
	if _, err := conn.ExecContext(ctx, `PRAGMA auto_vacuum = INCREMENTAL`); err != nil {
		return false, err
	}
	if _, err := conn.ExecContext(ctx, `VACUUM`); err != nil {
		return false, err
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		log.Printf("sqlitedb: %s: checkpoint after the VACUUM: %v", path, err)
	}
	after, _ := databaseSize(db)
	log.Printf("sqlitedb: %s: incremental vacuum on; %d MB -> %d MB in %s", path, size>>20, after>>20, time.Since(started).Round(time.Millisecond))
	return true, nil
}

// diskFree is freeDisk; a variable only so a test can say the disk is full.
var diskFree = freeDisk

// databaseSize is the database's size in bytes: its pages times their size.
func databaseSize(db *sql.DB) (int64, error) {
	var pages, pageSize int64
	if err := db.QueryRow(`PRAGMA page_count`).Scan(&pages); err != nil {
		return 0, err
	}
	if err := db.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		return 0, err
	}
	return pages * pageSize, nil
}

// KeepVacuumed gives freed pages back to the filesystem every interval, a batch at a time (IncrementalVacuum), until
// ctx ends. A database whose rows come and go (calls, steps, chat) otherwise only grows.
func KeepVacuumed(ctx context.Context, db *sql.DB, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := IncrementalVacuum(db, 2000); err != nil {
				log.Printf("sqlitedb: incremental vacuum: %v", err)
			}
		}
	}
}

// IncrementalVacuum frees up to pages unused database pages back to the host filesystem.
// If pages <= 0, a default batch of 100 pages is reclaimed.
func IncrementalVacuum(db *sql.DB, pages int) error {
	if pages <= 0 {
		pages = 100
	}
	_, err := db.Exec(fmt.Sprintf("PRAGMA incremental_vacuum(%d)", pages))
	return err
}

// OpenMemory opens a private in-memory database, for tests. Its pool is held
// to one connection deliberately, unlike Open's: SQLite gives each connection
// to an in-memory database its own private database unless the DSN opts into
// a shared cache, so a second connection here would see an empty database
// instead of the first connection's tables and rows.
func OpenMemory() (*sql.DB, error) {
	return open("file::memory:?_pragma=foreign_keys(ON)", 1)
}

func open(dsn string, maxConns int) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlitedb: open: %w", err)
	}

	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
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

// Scanner is what a row is read from: a *sql.Row or *sql.Rows.
type Scanner = interface{ Scan(dest ...any) error }

// QueryAll runs query on q (a *sql.DB or *sql.Tx) and returns every row as
// scan reads it - an empty, non-nil slice when there are none. The rows are
// closed before it returns, so the caller may run its next statement at once
// (see Open); scan itself must not run one.
func QueryAll[T any](q interface {
	Query(query string, args ...any) (*sql.Rows, error)
}, scan func(Scanner) (T, error), query string, args ...any) ([]T, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]T, 0)
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}

	return out, rows.Err()
}

// NewID returns prefix followed by n random bytes in hex: a row key that is
// unguessable and never collides with one made in the same instant.
func NewID(prefix string, n int) string {
	raw := make([]byte, n)
	_, _ = rand.Read(raw) // never fails: crypto/rand crashes the program instead

	return prefix + hex.EncodeToString(raw)
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
