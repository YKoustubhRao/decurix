// Package session holds each UI conversation's real turn-by-turn history
// in memory, so a second request in the same session actually has the
// first one available to it. Real bug this fixes, found live: general.go
// sent only the current request with zero history, so "what's my name"
// right after "my name is X" got an honest "I don't have that" - the
// model wasn't lying, the system genuinely never gave it anything to
// remember with.
package session

import (
	"sync"

	"thebot/internal/llm"
)

// maxHistoryMessages caps how many turns get threaded into a new call -
// bounded, not unlimited growth, while still covering a real, normal
// conversation.
const maxHistoryMessages = 40

// maxSessions bounds how many sessions are kept: past it the one used longest ago is dropped (review T-24 - a soul
// that lives for months kept every session it ever saw).
const maxSessions = 500

// Store is every session's turns, in memory, bounded per session and in number.
type Store struct {
	mu       sync.Mutex
	sessions map[string][]llm.Message
	used     map[string]int64 // the order each session was last appended to
	clock    int64
}

// NewStore is an empty Store.
func NewStore() *Store {
	return &Store{sessions: map[string][]llm.Message{}, used: map[string]int64{}}
}

// Append records one real turn (user or assistant) into a session -
// no-op for an empty sessionID, so callers that never got a real session
// id from the client don't panic or silently create a "" bucket.
func (s *Store) Append(sessionID string, msg llm.Message) {
	if sessionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	history := append(s.sessions[sessionID], msg)
	if len(history) > maxHistoryMessages {
		history = history[len(history)-maxHistoryMessages:]
	}
	s.sessions[sessionID] = history
	s.clock++
	s.used[sessionID] = s.clock
	if len(s.sessions) > maxSessions {
		oldest, at := "", int64(0)
		for id, t := range s.used {
			if oldest == "" || t < at {
				oldest, at = id, t
			}
		}
		delete(s.sessions, oldest)
		delete(s.used, oldest)
	}
}

// History returns a real copy of the session's turns so far - safe for a
// caller to append its own new turn onto without mutating the store.
func (s *Store) History(sessionID string) []llm.Message {
	if sessionID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	history := s.sessions[sessionID]
	out := make([]llm.Message, len(history))
	copy(out, history)
	return out
}
