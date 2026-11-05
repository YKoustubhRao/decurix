// Package outbox holds the emails agents prepared for a person to send.
//
// An agent can draft an email (a customer statement, a payment reminder, the
// monthly MIS report) but never send one: sending a message on someone's
// behalf is exactly the action every harness is told to stop before. So the
// azure harness's draft_email puts it here, the Triggers page lists it, and
// only a person's Send (httpapi, humansOnly) passes it to a connected
// mailbox's SMTP. A draft is marked, never deleted, so what went out stays
// on record next to the task that prepared it.
//
// Depends on nothing internal but sqlitedb.
package outbox

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// Statuses a draft moves through. Sending is the claim a Send takes, so two
// clicks cannot send one email twice.
const (
	StatusDraft     = "draft"
	StatusSending   = "sending"
	StatusSent      = "sent"
	StatusFailed    = "failed"
	StatusDiscarded = "discarded"
)

// Limits on what an agent may put here.
const (
	MaxRecipients     = 50
	MaxBodyChars      = 200_000
	MaxAttachments    = 20
	MaxAttachmentSize = 20 << 20 // all files of one draft together

	// MaxOpenDrafts and MaxOpenFileBytes bound what one desktop may leave
	// waiting for a person (drafts not yet sent or discarded, and their
	// files): an agent in a loop must not grow the database without end
	// (review 2026-11-03, B-28).
	MaxOpenDrafts    = 100
	MaxOpenFileBytes = 200 << 20
)

// openFileBytes is MaxOpenFileBytes, a variable for tests.
var openFileBytes = MaxOpenFileBytes

// ErrNotFound is a draft that does not exist.
var ErrNotFound = errors.New("outbox: no such draft")

// ErrInvalid is a draft an agent asked for that cannot be kept.
var ErrInvalid = errors.New("outbox: invalid draft")

// ErrNotDraft is a Send or Discard of a draft that is no longer one.
var ErrNotDraft = errors.New("outbox: that email is no longer a draft")

// File is one attachment.
type File struct {
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Size        int    `json:"size"`
	Data        []byte `json:"-"`
}

// Draft is one prepared email.
type Draft struct {
	ID        string    `json:"id"`
	DesktopID string    `json:"desktop_id"`
	TaskID    string    `json:"task_id"`
	To        []string  `json:"to"`
	Cc        []string  `json:"cc"`
	Subject   string    `json:"subject"`
	Body      string    `json:"body"`
	Files     []File    `json:"files"`
	Status    string    `json:"status"`
	Note      string    `json:"note"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

var schema = []string{
	`CREATE TABLE outbox_drafts (
		id          TEXT PRIMARY KEY,
		desktop_id  TEXT NOT NULL,
		task_id     TEXT NOT NULL,
		recipients  TEXT NOT NULL,
		cc          TEXT NOT NULL,
		subject     TEXT NOT NULL,
		body        TEXT NOT NULL,
		status      TEXT NOT NULL,
		note        TEXT NOT NULL DEFAULT '',
		created_ns  INTEGER NOT NULL,
		updated_ns  INTEGER NOT NULL
	)`,
	`CREATE TABLE outbox_files (
		draft_id     TEXT NOT NULL,
		position     INTEGER NOT NULL,
		name         TEXT NOT NULL,
		content_type TEXT NOT NULL,
		data         BLOB NOT NULL,
		PRIMARY KEY (draft_id, position)
	)`,
	`CREATE INDEX outbox_drafts_created ON outbox_drafts (created_ns)`,
}

// Store keeps drafts in SQLite.
type Store struct {
	db *sql.DB
	// mu makes a status change (read, check, write) one step.
	mu sync.Mutex
}

// New migrates the outbox's tables and returns its store.
func New(db *sql.DB) (*Store, error) {
	if err := sqlitedb.Migrate(db, "outbox", schema); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// Add keeps a new draft and returns it with its ID.
func (s *Store) Add(d Draft) (Draft, error) {
	d.To, d.Cc = clean(d.To), clean(d.Cc)
	d.Subject = strings.TrimSpace(d.Subject)

	switch {
	case len(d.To) == 0:
		return Draft{}, fmt.Errorf("%w: no recipient", ErrInvalid)
	case len(d.To)+len(d.Cc) > MaxRecipients:
		return Draft{}, fmt.Errorf("%w: too many recipients", ErrInvalid)
	case len(d.Body) > MaxBodyChars:
		return Draft{}, fmt.Errorf("%w: body too long", ErrInvalid)
	case len(d.Files) > MaxAttachments:
		return Draft{}, fmt.Errorf("%w: too many attachments", ErrInvalid)
	}

	total := 0
	for _, f := range d.Files {
		total += len(f.Data)
	}
	if total > MaxAttachmentSize {
		return Draft{}, fmt.Errorf("%w: attachments too large", ErrInvalid)
	}

	d.ID = sqlitedb.NewID("mail_", 8)
	d.Status = StatusDraft
	d.CreatedAt = time.Now()
	d.UpdatedAt = d.CreatedAt

	to, _ := json.Marshal(d.To)
	cc, _ := json.Marshal(d.Cc)

	// The count and the insert are one step, so two drafts at once cannot both
	// pass the last free place.
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return Draft{}, err
	}
	defer tx.Rollback()

	var open, openBytes int
	if err := tx.QueryRow(`SELECT COUNT(*), COALESCE((SELECT SUM(LENGTH(f.data)) FROM outbox_files f JOIN outbox_drafts o ON o.id = f.draft_id
			WHERE o.desktop_id = ? AND o.status IN (?, ?)), 0)
		FROM outbox_drafts WHERE desktop_id = ? AND status IN (?, ?)`,
		d.DesktopID, StatusDraft, StatusFailed, d.DesktopID, StatusDraft, StatusFailed).Scan(&open, &openBytes); err != nil {
		return Draft{}, fmt.Errorf("outbox: add: %w", err)
	}
	switch {
	case open >= MaxOpenDrafts:
		return Draft{}, fmt.Errorf("%w: this desktop already has %d emails waiting for a person; send or discard some first", ErrInvalid, open)
	case openBytes+total > openFileBytes:
		return Draft{}, fmt.Errorf("%w: the files of this desktop's waiting emails would pass %d MB; send or discard some first", ErrInvalid, openFileBytes>>20)
	}

	if _, err := tx.Exec(`INSERT INTO outbox_drafts (id, desktop_id, task_id, recipients, cc, subject, body, status, created_ns, updated_ns)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.DesktopID, d.TaskID, string(to), string(cc), d.Subject, d.Body, d.Status,
		sqlitedb.ToNS(d.CreatedAt), sqlitedb.ToNS(d.UpdatedAt)); err != nil {
		return Draft{}, fmt.Errorf("outbox: add: %w", err)
	}

	for i, f := range d.Files {
		if _, err := tx.Exec(`INSERT INTO outbox_files (draft_id, position, name, content_type, data) VALUES (?, ?, ?, ?, ?)`,
			d.ID, i, f.Name, f.ContentType, f.Data); err != nil {
			return Draft{}, fmt.Errorf("outbox: add: %w", err)
		}
		d.Files[i].Size = len(f.Data)
	}

	return d, tx.Commit()
}

const draftColumns = `id, desktop_id, task_id, recipients, cc, subject, body, status, note, created_ns, updated_ns`

// List returns the newest drafts first, with their files' names (no data).
func (s *Store) List(limit int) ([]Draft, error) {
	if limit <= 0 {
		limit = 100
	}

	out, err := sqlitedb.QueryAll(s.db, scan, `SELECT `+draftColumns+` FROM outbox_drafts ORDER BY created_ns DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("outbox: list: %w", err)
	}

	for i := range out {
		files, err := s.files(out[i].ID, false)
		if err != nil {
			return nil, err
		}
		out[i].Files = files
	}

	return out, nil
}

// Get returns one draft with its files' contents.
func (s *Store) Get(id string) (Draft, error) {
	d, err := scan(s.db.QueryRow(`SELECT `+draftColumns+` FROM outbox_drafts WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Draft{}, ErrNotFound
	}
	if err != nil {
		return Draft{}, err
	}

	d.Files, err = s.files(id, true)
	return d, err
}

func (s *Store) files(id string, withData bool) ([]File, error) {
	rows, err := s.db.Query(`SELECT name, content_type, data FROM outbox_files WHERE draft_id = ? ORDER BY position`, id)
	if err != nil {
		return nil, fmt.Errorf("outbox: files: %w", err)
	}
	defer rows.Close()

	var out []File
	for rows.Next() {
		var f File
		if err := rows.Scan(&f.Name, &f.ContentType, &f.Data); err != nil {
			return nil, err
		}
		f.Size = len(f.Data)
		if !withData {
			f.Data = nil
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func scan(rows sqlitedb.Scanner) (Draft, error) {
	var d Draft
	var to, cc string
	var created, updated int64

	if err := rows.Scan(&d.ID, &d.DesktopID, &d.TaskID, &to, &cc, &d.Subject, &d.Body, &d.Status, &d.Note, &created, &updated); err != nil {
		return Draft{}, fmt.Errorf("outbox: %w", err)
	}

	_ = json.Unmarshal([]byte(to), &d.To)
	_ = json.Unmarshal([]byte(cc), &d.Cc)
	// No recipients is an empty list, never null: clients add the lists together.
	if d.To == nil {
		d.To = []string{}
	}
	if d.Cc == nil {
		d.Cc = []string{}
	}
	d.CreatedAt, d.UpdatedAt = sqlitedb.FromNS(created), sqlitedb.FromNS(updated)

	return d, nil
}

// Claim moves a draft (or one whose send failed) to sending, and returns it
// with its files: the one step that makes a double click send once.
func (s *Store) Claim(id string) (Draft, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.Exec(`UPDATE outbox_drafts SET status = ?, note = '', updated_ns = ? WHERE id = ? AND status IN (?, ?)`,
		StatusSending, sqlitedb.ToNS(time.Now()), id, StatusDraft, StatusFailed)
	if err != nil {
		return Draft{}, fmt.Errorf("outbox: claim: %w", err)
	}

	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := s.Get(id); err != nil {
			return Draft{}, err
		}
		return Draft{}, ErrNotDraft
	}

	return s.Get(id)
}

// Finish records how a claimed send ended.
func (s *Store) Finish(id string, sendErr error) error {
	status, note := StatusSent, ""
	if sendErr != nil {
		status, note = StatusFailed, sendErr.Error()
	}

	_, err := s.db.Exec(`UPDATE outbox_drafts SET status = ?, note = ?, updated_ns = ? WHERE id = ? AND status = ?`,
		status, note, sqlitedb.ToNS(time.Now()), id, StatusSending)
	return err
}

// Discard drops a draft nobody will send.
func (s *Store) Discard(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.Exec(`UPDATE outbox_drafts SET status = ?, updated_ns = ? WHERE id = ? AND status IN (?, ?)`,
		StatusDiscarded, sqlitedb.ToNS(time.Now()), id, StatusDraft, StatusFailed)
	if err != nil {
		return fmt.Errorf("outbox: discard: %w", err)
	}

	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := s.Get(id); err != nil {
			return err
		}
		return ErrNotDraft
	}

	return nil
}

// RecoverInterrupted marks drafts left "sending" by a control plane that
// stopped mid-send as failed, never resending on its own: the email may
// already have left.
func (s *Store) RecoverInterrupted() error {
	_, err := s.db.Exec(`UPDATE outbox_drafts SET status = ?, note = ? WHERE status = ?`,
		StatusFailed, "Decurix stopped while sending this - check the Sent folder before sending it again.", StatusSending)
	return err
}

// Waiting counts the drafts waiting for a person.
func (s *Store) Waiting() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM outbox_drafts WHERE status = ?`, StatusDraft).Scan(&n)
	return n, err
}

func clean(addrs []string) []string {
	var out []string
	for _, a := range addrs {
		for _, part := range strings.FieldsFunc(a, func(r rune) bool { return r == ',' || r == ';' }) {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}
