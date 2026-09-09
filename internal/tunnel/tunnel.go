// Package tunnel holds the in-memory record of the current Cloudflare quick
// tunnel URL, if any, so the dashboard can surface it for mobile access.
package tunnel

import "sync"

// Store is an in-memory, mutex-guarded holder of the current tunnel URL.
type Store struct {
	mu  sync.Mutex
	url string
}

// NewStore creates an empty tunnel Store.
func NewStore() *Store {
	return &Store{}
}

// Set records the current tunnel URL.
func (s *Store) Set(url string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.url = url
}

// Get returns the current tunnel URL, or "" if none has been recorded.
func (s *Store) Get() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.url
}
