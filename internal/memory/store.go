// Package memory holds worker memory entries: reusable workflows or
// decisions learned while a desktop worker was operating. Entries are kept in
// the control plane's SQLite database, so they outlive both a restart and the
// desktop they were learned on.
package memory

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// WorkerMemoryEntry stores a reusable workflow or decision learned while a desktop worker was operating.
type WorkerMemoryEntry struct {
	ID        string    `json:"id"`
	DesktopID string    `json:"desktop_id"`
	Title     string    `json:"title"`
	Summary   string    `json:"summary"`
	Category  string    `json:"category"`
	CreatedAt time.Time `json:"created_at"`
}

var schema = []string{
	`CREATE TABLE worker_memory (
		id         TEXT PRIMARY KEY,
		desktop_id TEXT NOT NULL,
		title      TEXT NOT NULL,
		summary    TEXT NOT NULL,
		category   TEXT NOT NULL,
		created_ns INTEGER NOT NULL
	)`,
	`CREATE INDEX worker_memory_desktop ON worker_memory (desktop_id, created_ns)`,
	// worker_a/worker_b are always stored in canonicalized order (see
	// linkKey) so a link between two desktops is one row regardless of
	// which side asked for it, and re-linking the same pair is a no-op
	// rather than a duplicate.
	`CREATE TABLE worker_memory_links (
		worker_a   TEXT NOT NULL,
		worker_b   TEXT NOT NULL,
		created_ns INTEGER NOT NULL,
		PRIMARY KEY (worker_a, worker_b)
	)`,
}

// Store is a SQLite-backed collection of WorkerMemoryEntry, keyed by desktop ID.
type Store struct {
	db *sql.DB
}

// NewStore opens the memory Store on db, creating its table if needed.
func NewStore(db *sql.DB) (*Store, error) {
	if err := sqlitedb.Migrate(db, "memory", schema); err != nil {
		return nil, err
	}

	return &Store{db: db}, nil
}

// ErrInvalid marks input Record refused - the caller's mistake, which the
// HTTP layer answers with a 400 rather than an internal error.
var ErrInvalid = errors.New("invalid memory entry")

// Length caps. A worker's memory is read back into every prompt its desktop
// builds, so an entry that is not bounded is a tax on every later task.
const (
	maxDesktopIDLen = 128
	maxTitleLen     = 200
	maxSummaryLen   = 4000
)

// Record validates and stores a new worker memory entry. It does not persist
// the entry to a desktop container — callers that need that should follow up
// with FormatForDesktopFile and the desktop package's PersistMemoryFile.
func (s *Store) Record(desktopID, title, summary, category string) (WorkerMemoryEntry, error) {
	if strings.TrimSpace(desktopID) == "" {
		return WorkerMemoryEntry{}, fmt.Errorf("%w: desktop_id is required", ErrInvalid)
	}

	title = strings.TrimSpace(title)
	summary = strings.TrimSpace(summary)
	category = strings.TrimSpace(category)

	if title == "" {
		return WorkerMemoryEntry{}, fmt.Errorf("%w: title is required", ErrInvalid)
	}

	if summary == "" {
		return WorkerMemoryEntry{}, fmt.Errorf("%w: summary is required", ErrInvalid)
	}

	if len(desktopID) > maxDesktopIDLen || len(title) > maxTitleLen || len(summary) > maxSummaryLen {
		return WorkerMemoryEntry{}, fmt.Errorf("%w: field too long", ErrInvalid)
	}

	if category == "" {
		category = "workflow"
	}

	entry := WorkerMemoryEntry{
		ID:        fmt.Sprintf("memory-%d", time.Now().UnixNano()),
		DesktopID: strings.TrimSpace(desktopID),
		Title:     title,
		Summary:   summary,
		Category:  category,
		CreatedAt: time.Now().UTC(),
	}

	if _, err := s.db.Exec(
		`INSERT INTO worker_memory (id, desktop_id, title, summary, category, created_ns)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		entry.ID, entry.DesktopID, entry.Title, entry.Summary, entry.Category,
		sqlitedb.ToNS(entry.CreatedAt),
	); err != nil {
		return WorkerMemoryEntry{}, fmt.Errorf("memory: recording entry: %w", err)
	}

	return entry, nil
}

// maxListedEntries bounds one List call. Worker memory accumulates forever -
// nothing here deletes or archives an entry - so an unfiltered List (the
// dashboard's own call, on every page load) stays flat-cost as the fleet's
// history grows instead of materializing every entry ever recorded.
const maxListedEntries = 2000

// List returns worker memory entries, most recent first, up to
// maxListedEntries. When desktopID is empty, entries for every desktop are
// returned.
func (s *Store) List(desktopID string) []WorkerMemoryEntry {
	query := `SELECT id, desktop_id, title, summary, category, created_ns FROM worker_memory`
	args := []any{}

	if desktopID != "" {
		query += ` WHERE desktop_id = ?`
		args = append(args, desktopID)
	}

	query += ` ORDER BY created_ns DESC LIMIT ?`
	args = append(args, maxListedEntries)

	entries := make([]WorkerMemoryEntry, 0)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		log.Printf("memory: list: %v", err)
		return entries
	}
	defer rows.Close()

	for rows.Next() {
		var entry WorkerMemoryEntry
		var created int64

		if err := rows.Scan(&entry.ID, &entry.DesktopID, &entry.Title, &entry.Summary, &entry.Category, &created); err != nil {
			log.Printf("memory: list: %v", err)
			continue
		}

		entry.CreatedAt = sqlitedb.FromNS(created)
		entries = append(entries, entry)
	}

	return entries
}

// linkKey canonicalizes a pair of desktop IDs into a stable (a, b) order so
// a link is stored and looked up the same way regardless of which desktop
// asked for it.
func linkKey(x, y string) (a, b string) {
	if x <= y {
		return x, y
	}
	return y, x
}

// Link records that two workers share L1 memory going forward: a future
// entry recorded for either one is also persisted into the other's desktop
// (see httpapi's desktopMemoryHandler). It is idempotent - linking an
// already-linked pair changes nothing. Sharing is deliberately a link
// between two desktop IDs, not a merge of their entries: each entry keeps
// recording under the desktop it was actually learned on.
func (s *Store) Link(desktopA, desktopB string) error {
	desktopA = strings.TrimSpace(desktopA)
	desktopB = strings.TrimSpace(desktopB)

	if desktopA == "" || desktopB == "" {
		return fmt.Errorf("both desktop IDs are required")
	}

	if desktopA == desktopB {
		return fmt.Errorf("a worker cannot share memory with itself")
	}

	a, b := linkKey(desktopA, desktopB)

	if _, err := s.db.Exec(
		`INSERT OR IGNORE INTO worker_memory_links (worker_a, worker_b, created_ns) VALUES (?, ?, ?)`,
		a, b, sqlitedb.ToNS(time.Now()),
	); err != nil {
		return fmt.Errorf("memory: link: %w", err)
	}

	return nil
}

// Unlink stops sharing L1 memory between two workers. Entries already
// copied into either desktop's container stay put - this only stops future
// entries from crossing over.
func (s *Store) Unlink(desktopA, desktopB string) error {
	a, b := linkKey(strings.TrimSpace(desktopA), strings.TrimSpace(desktopB))

	if _, err := s.db.Exec(
		`DELETE FROM worker_memory_links WHERE worker_a = ? AND worker_b = ?`,
		a, b,
	); err != nil {
		return fmt.Errorf("memory: unlink: %w", err)
	}

	return nil
}

// LinkedWorkers returns the desktop IDs sharing L1 memory with desktopID,
// in no particular order.
func (s *Store) LinkedWorkers(desktopID string) []string {
	desktopID = strings.TrimSpace(desktopID)
	linked := make([]string, 0)

	rows, err := s.db.Query(
		`SELECT worker_a, worker_b FROM worker_memory_links WHERE worker_a = ? OR worker_b = ?`,
		desktopID, desktopID,
	)
	if err != nil {
		log.Printf("memory: linked workers: %v", err)
		return linked
	}
	defer rows.Close()

	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			log.Printf("memory: linked workers: %v", err)
			continue
		}

		if a == desktopID {
			linked = append(linked, b)
		} else {
			linked = append(linked, a)
		}
	}

	return linked
}

// LinkedWorkersMap returns every desktop's linked worker IDs in one query,
// keyed by desktop ID - what the dashboard uses to build its whole page's
// worth of links without one LinkedWorkers query per desktop listed.
func (s *Store) LinkedWorkersMap() map[string][]string {
	linked := map[string][]string{}

	rows, err := s.db.Query(`SELECT worker_a, worker_b FROM worker_memory_links`)
	if err != nil {
		log.Printf("memory: linked workers map: %v", err)
		return linked
	}
	defer rows.Close()

	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			log.Printf("memory: linked workers map: %v", err)
			continue
		}

		linked[a] = append(linked[a], b)
		linked[b] = append(linked[b], a)
	}

	return linked
}

// FormatForDesktopFile renders a memory entry as the flat text file that gets
// persisted into a desktop's memory directory, and returns the filename to
// use for it.
func FormatForDesktopFile(entry WorkerMemoryEntry) (filename string, content string) {
	// entry.ID ("memory-<unixnano>") rather than a fresh, second-granularity
	// timestamp: two entries recorded for the same desktop within the same
	// second used to collide and one silently overwrote the other once
	// persisted. The ID is already unique (and, since it carries the
	// nanosecond timestamp, still sorts chronologically the way
	// memory_context.py's glob().sort() on the filename expects).
	filename = entry.ID + ".txt"

	content = fmt.Sprintf(
		`Title: %s
Category: %s
DesktopID: %s
CreatedAt: %s

%s`,
		strings.ReplaceAll(entry.Title, "\n", " "),
		strings.ReplaceAll(entry.Category, "\n", " "),
		strings.ReplaceAll(entry.DesktopID, "\n", " "),
		entry.CreatedAt.Format(time.RFC3339),
		strings.TrimSpace(entry.Summary),
	)

	return filename, content
}
