// Package devices keeps the phones and laptops a person has paired with the
// gated listener (internal/lanaccess): a device that entered the access code
// once and chose "Remember this device" holds a long-lived token instead of
// a session that dies with the next code rotation.
//
// Only a SHA-256 of each token is stored, so a copied decurix.db cannot be
// replayed as a paired device. A device is revoked, never expired: a phone
// someone paired should keep working until they say otherwise.
package devices

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// maxNameLen caps a device's name, which the person types on the gate page.
const maxNameLen = 60

// seenEvery is how often a device's last-seen time is written back: every
// request passes through Valid, and one write per request is pointless.
const seenEvery = time.Minute

// ErrNotFound is returned when revoking a device that is not paired.
var ErrNotFound = errors.New("devices: no such device")

// Device is one paired phone or computer, as the Mobile menu lists it.
type Device struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	PairedAt time.Time `json:"paired_at"`
	LastSeen time.Time `json:"last_seen"`
}

var schema = []string{
	`CREATE TABLE paired_devices (
		id         TEXT PRIMARY KEY,
		token_hash TEXT NOT NULL UNIQUE,
		name       TEXT NOT NULL,
		paired_ns  INTEGER NOT NULL,
		seen_ns    INTEGER NOT NULL
	)`,
	// More tokens for one device: a phone's browser and the phone link it
	// sets up (internal/phoneagent) are one device in the list, each with
	// its own sign-in.
	`CREATE TABLE device_tokens (
		token_hash TEXT PRIMARY KEY,
		device_id  TEXT NOT NULL
	)`,
}

// Store is the SQLite-backed list of paired devices. It satisfies
// lanaccess.DeviceRegistry.
type Store struct {
	db *sql.DB

	// mu guards seen, the in-memory throttle on last-seen writes, and
	// onRevoke.
	mu       sync.Mutex
	seen     map[string]time.Time
	onRevoke []func(id string)
}

// OnRevoke registers f to be called with a device's id once it is unpaired,
// by Revoke or Forget: what that device holds open beyond its next request
// (a phone's live link) is ended there (review 2026-11-03, B-18).
func (s *Store) OnRevoke(f func(id string)) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.onRevoke = append(s.onRevoke, f)
}

// NewStore opens the devices Store on db, creating its table if needed.
func NewStore(db *sql.DB) (*Store, error) {
	if err := sqlitedb.Migrate(db, "devices", schema); err != nil {
		return nil, err
	}

	return &Store{db: db, seen: make(map[string]time.Time)}, nil
}

// hashToken returns the stored form of a device token.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// randomHex returns n random bytes, hex-encoded.
func randomHex(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}

	return hex.EncodeToString(raw), nil
}

// cleanName trims a typed device name to something the menu can show.
func cleanName(name string) string {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		name = "Device"
	}

	if runes := []rune(name); len(runes) > maxNameLen {
		name = string(runes[:maxNameLen])
	}

	return name
}

// Pair records a new device and returns the token it will present from now on.
func (s *Store) Pair(name string) (token string, err error) {
	token, err = randomHex(32)
	if err != nil {
		return "", fmt.Errorf("devices: generating a token: %w", err)
	}

	id, err := randomHex(8)
	if err != nil {
		return "", fmt.Errorf("devices: generating an id: %w", err)
	}

	now := sqlitedb.ToNS(time.Now())

	_, err = s.db.Exec(
		`INSERT INTO paired_devices (id, token_hash, name, paired_ns, seen_ns) VALUES (?, ?, ?, ?, ?)`,
		id, hashToken(token), cleanName(name), now, now,
	)
	if err != nil {
		return "", fmt.Errorf("devices: pairing: %w", err)
	}

	return token, nil
}

// Valid reports whether token belongs to a paired device, noting that the
// device was seen (at most once per seenEvery).
func (s *Store) Valid(token string) bool {
	if token == "" {
		return false
	}

	id, ok := s.deviceFor(token)
	if !ok {
		return false
	}

	now := time.Now()

	s.mu.Lock()
	due := now.Sub(s.seen[id]) >= seenEvery
	if due {
		s.seen[id] = now
	}
	s.mu.Unlock()

	if due {
		// Best-effort: failing to note the time must not refuse the device.
		_, _ = s.db.Exec(`UPDATE paired_devices SET seen_ns = ? WHERE id = ?`, sqlitedb.ToNS(now), id)
	}

	return true
}

// DeviceID returns the id of the paired device token belongs to, whichever of its tokens it is: how the gate tracks
// what a device holds open, to end it when the device is unpaired (lanaccess.CloseDevice).
func (s *Store) DeviceID(token string) (string, bool) { return s.deviceFor(token) }

// deviceFor returns the id of the device token belongs to, whichever of its
// tokens it is.
func (s *Store) deviceFor(token string) (string, bool) {
	var id string

	// An extra token counts only while its device is paired: one left behind
	// by a revoke that raced AddToken must not pass the gate unlisted.
	err := s.db.QueryRow(`SELECT id FROM paired_devices WHERE token_hash = ?
		UNION ALL SELECT t.device_id FROM device_tokens t JOIN paired_devices d ON d.id = t.device_id
		WHERE t.token_hash = ? LIMIT 1`,
		hashToken(token), hashToken(token)).Scan(&id)

	return id, err == nil
}

// AddToken gives the paired device id another token of its own - for the
// phone link a phone's browser sets up - and returns it. Revoking the device
// ends every one of its tokens.
func (s *Store) AddToken(id string) (string, error) {
	token, err := randomHex(32)
	if err != nil {
		return "", fmt.Errorf("devices: generating a token: %w", err)
	}

	// One statement, so a Revoke between a check and the insert cannot leave
	// a token for a device that is gone.
	res, err := s.db.Exec(`INSERT INTO device_tokens (token_hash, device_id)
		SELECT ?, ? WHERE EXISTS (SELECT 1 FROM paired_devices WHERE id = ?)`, hashToken(token), id, id)
	if err != nil {
		return "", fmt.Errorf("devices: adding a token: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", ErrNotFound
	}

	return token, nil
}

// Lookup returns the paired device holding token - how a phone's link to
// the control plane (internal/phonehost) knows which phone it is.
func (s *Store) Lookup(token string) (Device, bool) {
	if !s.Valid(token) {
		return Device{}, false
	}

	id, ok := s.deviceFor(token)
	if !ok {
		return Device{}, false
	}

	var d Device
	var paired, seen int64

	err := s.db.QueryRow(`SELECT id, name, paired_ns, seen_ns FROM paired_devices WHERE id = ?`, id).
		Scan(&d.ID, &d.Name, &paired, &seen)
	if err != nil {
		return Device{}, false
	}

	d.PairedAt, d.LastSeen = sqlitedb.FromNS(paired), sqlitedb.FromNS(seen)

	return d, true
}

// Forget unpairs the device holding token - the device signing itself out,
// with every token it has.
func (s *Store) Forget(token string) error {
	if token == "" {
		return nil
	}

	id, ok := s.deviceFor(token)
	if !ok {
		return nil
	}

	if err := s.Revoke(id); err != nil && err != ErrNotFound {
		return err
	}

	return nil
}

// Revoke unpairs the device with this id - the Mobile menu's Revoke button.
// The device and every token it has go in one transaction.
func (s *Store) Revoke(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	result, err := tx.Exec(`DELETE FROM paired_devices WHERE id = ?`, id)
	if err != nil {
		return err
	}

	if _, err := tx.Exec(`DELETE FROM device_tokens WHERE device_id = ?`, id); err != nil {
		return err
	}

	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	s.mu.Lock()
	delete(s.seen, id)
	hooks := append([]func(string){}, s.onRevoke...)
	s.mu.Unlock()

	for _, f := range hooks {
		f(id)
	}

	return nil
}

// List returns every paired device, most recently seen first.
func (s *Store) List() ([]Device, error) {
	rows, err := s.db.Query(`SELECT id, name, paired_ns, seen_ns FROM paired_devices ORDER BY seen_ns DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	list := []Device{}

	for rows.Next() {
		var d Device
		var paired, seen int64

		if err := rows.Scan(&d.ID, &d.Name, &paired, &seen); err != nil {
			return nil, err
		}

		d.PairedAt = sqlitedb.FromNS(paired)
		d.LastSeen = sqlitedb.FromNS(seen)
		list = append(list, d)
	}

	return list, rows.Err()
}
