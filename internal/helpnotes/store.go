// Package helpnotes holds the in-memory record of help requests submitted by
// desktop agents.
package helpnotes

import (
	"sync"
	"time"
)

// HelpNote is a support note submitted from inside a running desktop, tied to a specific desktop and VM.
type HelpNote struct {
	ID        string    `json:"id"`
	DesktopID string    `json:"desktop_id"`
	VMName    string    `json:"vm_name"`
	Filename  string    `json:"filename"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// Store is an in-memory, mutex-guarded collection of HelpNotes.
type Store struct {
	mu    sync.Mutex
	notes map[string]HelpNote
}

// NewStore creates an empty help-notes Store.
func NewStore() *Store {
	return &Store{
		notes: make(map[string]HelpNote),
	}
}

// Add records a help note.
func (s *Store) Add(note HelpNote) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.notes[note.ID] = note
}

// List returns all recorded help notes, in no particular order.
func (s *Store) List() []HelpNote {
	s.mu.Lock()
	defer s.mu.Unlock()

	notes := make([]HelpNote, 0, len(s.notes))

	for _, note := range s.notes {
		notes = append(notes, note)
	}

	return notes
}

// Close deletes a help note by ID. It is a no-op if the ID is not present.
func (s *Store) Close(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.notes, id)
}
