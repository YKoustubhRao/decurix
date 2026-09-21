// Package stepimages holds the before/after screenshots attached to the
// on-screen action steps a desktop reports — the audit trail a human expands
// under an answer, or opens on a task record, to see what the agent actually
// clicked.
//
// Images live in the control plane's SQLite database, so a task record's
// screenshots survive a restart. Screenshots are still the heaviest thing kept,
// so the store is bounded by total bytes and forgets the oldest images first.
// The full-resolution originals are not lost when that happens: every desktop
// keeps them on its own persistent volume (decurix_agent/computer/audit.py).
package stepimages

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"strings"
	"sync"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// DefaultMaxBytes bounds the whole store. At ~25 KB per half-size JPEG this is
// tens of thousands of screenshots across every desktop.
const DefaultMaxBytes = 1 << 30

// maxInputBytes and maxPixels refuse a payload before it is decoded: a PNG
// is compressed, and a small one can claim dimensions that would allocate
// gigabytes.
const (
	maxInputBytes = 8 << 20
	maxPixels     = 4096 * 4096
	jpegQuality   = 75
)

// ErrTooLarge is returned for an image past the input bounds.
var ErrTooLarge = errors.New("stepimages: image too large")

var schema = []string{
	`CREATE TABLE step_images (
		id         TEXT PRIMARY KEY,
		desktop_id TEXT NOT NULL,
		data       BLOB NOT NULL,
		size       INTEGER NOT NULL,
		created_ns INTEGER NOT NULL
	)`,
}

// Store is a SQLite-backed, byte-bounded set of screenshots keyed by random ID.
type Store struct {
	db *sql.DB

	// mu guards size, the running total that decides eviction, for the whole
	// insert-then-evict sequence.
	mu       sync.Mutex
	size     int
	maxBytes int
}

// NewStore opens the Store on db, holding at most maxBytes of images.
func NewStore(db *sql.DB, maxBytes int) (*Store, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}

	if err := sqlitedb.Migrate(db, "stepimages", schema); err != nil {
		return nil, err
	}

	store := &Store{db: db, maxBytes: maxBytes}

	if err := db.QueryRow(`SELECT COALESCE(SUM(size), 0) FROM step_images`).Scan(&store.size); err != nil {
		return nil, fmt.Errorf("stepimages: measuring store: %w", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	if err := store.evict(); err != nil {
		return nil, err
	}

	return store, nil
}

// Put decodes an incoming screenshot (PNG or JPEG), re-encodes it as JPEG, stores it for a desktop
// and returns its ID.
func (s *Store) Put(desktopID string, data []byte) (string, error) {
	if len(data) > maxInputBytes {
		return "", ErrTooLarge
	}

	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("stepimages: decoding image config: %w", err)
	}

	if config.Width*config.Height > maxPixels {
		return "", ErrTooLarge
	}

	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("stepimages: decoding image: %w", err)
	}

	return s.put(desktopID, img)
}

// put encodes and stores one decoded image.
func (s *Store) put(desktopID string, img image.Image) (string, error) {
	var encoded bytes.Buffer

	if err := jpeg.Encode(&encoded, img, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return "", fmt.Errorf("stepimages: encoding JPEG: %w", err)
	}

	id, err := newID()
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.db.Exec(
		`INSERT INTO step_images (id, desktop_id, data, size, created_ns) VALUES (?, ?, ?, ?, ?)`,
		id, desktopID, encoded.Bytes(), encoded.Len(), time.Now().UnixNano(),
	); err != nil {
		return "", fmt.Errorf("stepimages: storing image: %w", err)
	}

	s.size += encoded.Len()

	if err := s.evict(); err != nil {
		return "", err
	}

	return id, nil
}

// Get returns one stored JPEG.
func (s *Store) Get(id string) ([]byte, bool) {
	var data []byte

	if err := s.db.QueryRow(`SELECT data FROM step_images WHERE id = ?`, id).Scan(&data); err != nil {
		return nil, false
	}

	return data, true
}

// evictBatch is how many of the oldest images one eviction pass looks at.
const evictBatch = 64

// evict drops the oldest images until the store is within its bound. The
// caller must hold the lock.
func (s *Store) evict() error {
	for s.size > s.maxBytes {
		rows, err := s.db.Query(`SELECT id, size FROM step_images ORDER BY rowid LIMIT ?`, evictBatch)
		if err != nil {
			return fmt.Errorf("stepimages: evicting: %w", err)
		}

		type victim struct {
			id   string
			size int
		}

		var victims []victim

		for rows.Next() {
			var v victim
			if err := rows.Scan(&v.id, &v.size); err != nil {
				rows.Close()
				return fmt.Errorf("stepimages: evicting: %w", err)
			}
			victims = append(victims, v)
		}
		rows.Close()

		if len(victims) == 0 {
			s.size = 0
			return nil
		}

		var toDelete []any
		var placeholders []string
		deletedBytes := 0
		for _, v := range victims {
			toDelete = append(toDelete, v.id)
			placeholders = append(placeholders, "?")
			deletedBytes += v.size
			if s.size-deletedBytes <= s.maxBytes {
				break
			}
		}

		if len(toDelete) > 0 {
			query := fmt.Sprintf(`DELETE FROM step_images WHERE id IN (%s)`, strings.Join(placeholders, ","))
			if _, err := s.db.Exec(query, toDelete...); err != nil {
				return fmt.Errorf("stepimages: evicting: %w", err)
			}
			s.size -= deletedBytes
			_ = sqlitedb.IncrementalVacuum(s.db, len(toDelete)*2)
		}
	}

	return nil
}

// newID returns an unguessable identifier, so one desktop's screenshots are
// not enumerable by counting.
func newID() (string, error) {
	raw := make([]byte, 12)

	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("stepimages: generating id: %w", err)
	}

	return hex.EncodeToString(raw), nil
}
