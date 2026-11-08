// Package tunnel holds the in-memory record of the addresses this computer's
// own scripts (scripts/*/lib/lan-tunnel.sh, Lan-Tunnel.ps1) and Electron
// shell discover and report back for the dashboard's Mobile menu to display:
// the Cloudflare quick tunnel URL and the LAN address of the
// internal/lanaccess-gated listener, the only listener that is ever
// tunnelled or offered off this machine. The two primary-listener fields
// (the quick tunnel URL and the admin LAN URL) are legacy: nothing reports
// them any more, and they stay only so an old script's POST is still
// accepted. All of these are "how do I reach this control plane from
// somewhere else", discovered outside the Go process and reported in, not
// computed here.
package tunnel

import "sync"

// Store is an in-memory, mutex-guarded holder of the current tunnel URLs and
// the two LAN addresses.
type Store struct {
	mu           sync.Mutex
	url          string
	adminURL     string
	lanURL       string
	lanTunnelURL string
}

// NewStore creates an empty tunnel Store.
func NewStore() *Store {
	return &Store{}
}

// set records one of the addresses.
func (s *Store) set(field *string, url string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	*field = url
}

// get returns one of the addresses.
func (s *Store) get(field *string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return *field
}

// Set records the current Cloudflare tunnel URL.
func (s *Store) Set(url string) { s.set(&s.url, url) }

// Get returns the current Cloudflare tunnel URL, or "" if none has been
// recorded.
func (s *Store) Get() string { return s.get(&s.url) }

// SetAdminURL records the machine's LAN address for the primary listener -
// full, unauthenticated control, same as sitting at the machine itself.
func (s *Store) SetAdminURL(url string) { s.set(&s.adminURL, url) }

// AdminURL returns the current admin URL, or "" if none has been recorded.
func (s *Store) AdminURL() string { return s.get(&s.adminURL) }

// SetLANURL records the machine's LAN address for the internal/lanaccess
// -gated listener - reachable on the local network without the Cloudflare
// tunnel, but still behind the access code.
func (s *Store) SetLANURL(url string) { s.set(&s.lanURL, url) }

// LANURL returns the current gated-listener LAN URL, or "" if none has been
// recorded.
func (s *Store) LANURL() string { return s.get(&s.lanURL) }

// SetLANTunnelURL records the current Cloudflare quick tunnel URL for the
// internal/lanaccess-gated listener - what makes the gated URL reachable
// from anywhere, still behind the access code, instead of only on the local
// network like LANURL.
func (s *Store) SetLANTunnelURL(url string) { s.set(&s.lanTunnelURL, url) }

// LANTunnelURL returns the current gated-listener tunnel URL, or "" if none
// has been recorded.
func (s *Store) LANTunnelURL() string { return s.get(&s.lanTunnelURL) }
