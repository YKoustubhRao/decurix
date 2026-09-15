// Package tunnel holds the in-memory record of the addresses a host-side
// script (scripts/*/build.sh, rebuild.sh) discovers and reports back for the
// dashboard's Mobile menu to display: the current Cloudflare quick tunnel
// URL, and the machine's own LAN address for each of the two listeners
// cmd/flowera/main.go starts - the primary one (full, unauthenticated
// control) and the internal/lanaccess-gated one. All three are "how do I
// reach this control plane from somewhere else", discovered outside the Go
// process and reported in, not computed here.
package tunnel

import "sync"

// Store is an in-memory, mutex-guarded holder of the current tunnel URL and
// the two LAN addresses.
type Store struct {
	mu       sync.Mutex
	url      string
	adminURL string
	lanURL   string
}

// NewStore creates an empty tunnel Store.
func NewStore() *Store {
	return &Store{}
}

// Set records the current Cloudflare tunnel URL.
func (s *Store) Set(url string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.url = url
}

// Get returns the current Cloudflare tunnel URL, or "" if none has been
// recorded.
func (s *Store) Get() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.url
}

// SetAdminURL records the machine's LAN address for the primary listener -
// full, unauthenticated control, same as sitting at the machine itself.
func (s *Store) SetAdminURL(url string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.adminURL = url
}

// AdminURL returns the current admin URL, or "" if none has been recorded.
func (s *Store) AdminURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.adminURL
}

// SetLANURL records the machine's LAN address for the internal/lanaccess
// -gated listener - reachable on the local network without the Cloudflare
// tunnel, but still behind the access code.
func (s *Store) SetLANURL(url string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.lanURL = url
}

// LANURL returns the current gated-listener LAN URL, or "" if none has been
// recorded.
func (s *Store) LANURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.lanURL
}
