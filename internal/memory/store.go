// Package memory holds worker memory entries: reusable workflows or
// decisions learned while a desktop worker was operating. Entries are kept in
// the control plane's SQLite database, so they outlive both a restart and the
// desktop they were learned on.
package memory

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/YKoustubhRao/flowera/internal/sqlitedb"
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

// Record validates and stores a new worker memory entry. It does not persist
// the entry to a desktop container — callers that need that should follow up
// with FormatForDesktopFile and the desktop package's PersistMemoryFile.
func (s *Store) Record(desktopID, title, summary, category string) (WorkerMemoryEntry, error) {
	if strings.TrimSpace(desktopID) == "" {
		return WorkerMemoryEntry{}, fmt.Errorf("desktop_id is required")
	}

	title = strings.TrimSpace(title)
	summary = strings.TrimSpace(summary)
	category = strings.TrimSpace(category)

	if title == "" {
		return WorkerMemoryEntry{}, fmt.Errorf("title is required")
	}

	if summary == "" {
		return WorkerMemoryEntry{}, fmt.Errorf("summary is required")
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
