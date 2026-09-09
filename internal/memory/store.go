// Package memory holds worker memory entries: reusable workflows or
// decisions learned while a desktop worker was operating.
package memory

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
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

// Store is an in-memory, mutex-guarded collection of WorkerMemoryEntry, keyed by desktop ID.
type Store struct {
	mu      sync.Mutex
	entries map[string][]WorkerMemoryEntry
}

// NewStore creates an empty memory Store.
func NewStore() *Store {
	return &Store{
		entries: make(map[string][]WorkerMemoryEntry),
	}
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

	s.mu.Lock()
	defer s.mu.Unlock()

	s.entries[entry.DesktopID] = append(s.entries[entry.DesktopID], entry)

	return entry, nil
}

// List returns worker memory entries, most recent first. When desktopID is
// empty, entries for every desktop are returned.
func (s *Store) List(desktopID string) []WorkerMemoryEntry {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries := make([]WorkerMemoryEntry, 0)

	if desktopID != "" {
		entries = append(entries, s.entries[desktopID]...)
	} else {
		for _, memoryEntries := range s.entries {
			entries = append(entries, memoryEntries...)
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].CreatedAt.After(entries[j].CreatedAt)
	})

	return entries
}

// FormatForDesktopFile renders a memory entry as the flat text file that gets
// persisted into a desktop's memory directory, and returns the filename to
// use for it.
func FormatForDesktopFile(entry WorkerMemoryEntry) (filename string, content string) {
	filename = time.Now().Format("20060102150405") + ".txt"

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
