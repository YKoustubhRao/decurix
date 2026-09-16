// Package schedule stores recurring requests - the same content resubmitted
// to a desktop every time a cron expression next matches, the way a human
// retyping it and hitting Send would - and the Runner that fires them. A
// schedule is created from the requests page (see internal/httpapi's
// schedules.go), never from the chat: it is a fourth door onto the same
// Plan/deliver/Commit path the chat, the requests page and a query reply
// already share.
package schedule

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/YKoustubhRao/flowera/internal/sqlitedb"
)

// Schedule is a recurring request, or a one-time request held for a single
// future run (RunOnce), which fires exactly once and then disables itself
// rather than computing a next occurrence.
type Schedule struct {
	ID        string    `json:"id"`
	DesktopID string    `json:"desktop_id"`
	Content   string    `json:"content"`
	CronExpr  string    `json:"cron_expr"`
	RunOnce   bool      `json:"run_once"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	LastRunAt time.Time `json:"last_run_at"`
	NextRunAt time.Time `json:"next_run_at"`
}

var schema = []string{
	`CREATE TABLE schedules (
		id          TEXT PRIMARY KEY,
		desktop_id  TEXT NOT NULL,
		content     TEXT NOT NULL,
		cron_expr   TEXT NOT NULL,
		enabled     INTEGER NOT NULL,
		created_ns  INTEGER NOT NULL,
		last_run_ns INTEGER NOT NULL DEFAULT 0,
		next_run_ns INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX schedules_desktop ON schedules (desktop_id, created_ns)`,
	`CREATE INDEX schedules_due ON schedules (enabled, next_run_ns)`,
	`ALTER TABLE schedules ADD COLUMN run_once INTEGER NOT NULL DEFAULT 0`,
}

// Store is a SQLite-backed collection of Schedules.
type Store struct {
	db *sql.DB

	// now is the test seam - defaults to time.Now().UTC.
	now func() time.Time
}

// NewStore opens the schedule Store on db, creating its table if needed.
func NewStore(db *sql.DB) (*Store, error) {
	if err := sqlitedb.Migrate(db, "schedule", schema); err != nil {
		return nil, err
	}

	return &Store{db: db}, nil
}

// Create validates cronExpr and persists a new schedule. Its first run is the
// next time the expression matches, never immediately - the same way cron
// itself never runs a job the instant it is added.
func (s *Store) Create(desktopID, content, cronExpr string) (Schedule, error) {
	desktopID = strings.TrimSpace(desktopID)
	cronExpr = strings.TrimSpace(cronExpr)

	if desktopID == "" {
		return Schedule{}, fmt.Errorf("desktop id is required")
	}

	if strings.TrimSpace(content) == "" {
		return Schedule{}, fmt.Errorf("request text is required")
	}

	expr, err := Parse(cronExpr)
	if err != nil {
		return Schedule{}, err
	}

	now := s.clock()

	next, ok := expr.Next(now)
	if !ok {
		return Schedule{}, fmt.Errorf("cron expression %q never matches", cronExpr)
	}

	sch := Schedule{
		ID:        newID(),
		DesktopID: desktopID,
		Content:   content,
		CronExpr:  cronExpr,
		Enabled:   true,
		CreatedAt: now,
		NextRunAt: next,
	}

	if _, err := s.db.Exec(
		`INSERT INTO schedules (id, desktop_id, content, cron_expr, enabled, created_ns, last_run_ns, next_run_ns, run_once)
		 VALUES (?, ?, ?, ?, 1, ?, 0, ?, 0)`,
		sch.ID, sch.DesktopID, sch.Content, sch.CronExpr,
		sqlitedb.ToNS(sch.CreatedAt), sqlitedb.ToNS(sch.NextRunAt),
	); err != nil {
		return Schedule{}, fmt.Errorf("schedule: create: %w", err)
	}

	return sch, nil
}

// CreateOnce persists a request held for a single future run at runAt, rather
// than a recurring cron expression. It fires exactly once (see MarkRun) - the
// same "never immediately" rule as Create applies here too: runAt must be
// strictly after now.
func (s *Store) CreateOnce(desktopID, content string, runAt time.Time) (Schedule, error) {
	desktopID = strings.TrimSpace(desktopID)

	if desktopID == "" {
		return Schedule{}, fmt.Errorf("desktop id is required")
	}

	if strings.TrimSpace(content) == "" {
		return Schedule{}, fmt.Errorf("request text is required")
	}

	now := s.clock()
	runAt = runAt.UTC()

	if !runAt.After(now) {
		return Schedule{}, fmt.Errorf("run time must be in the future")
	}

	sch := Schedule{
		ID:        newID(),
		DesktopID: desktopID,
		Content:   content,
		RunOnce:   true,
		Enabled:   true,
		CreatedAt: now,
		NextRunAt: runAt,
	}

	if _, err := s.db.Exec(
		`INSERT INTO schedules (id, desktop_id, content, cron_expr, enabled, created_ns, last_run_ns, next_run_ns, run_once)
		 VALUES (?, ?, ?, '', 1, ?, 0, ?, 1)`,
		sch.ID, sch.DesktopID, sch.Content,
		sqlitedb.ToNS(sch.CreatedAt), sqlitedb.ToNS(sch.NextRunAt),
	); err != nil {
		return Schedule{}, fmt.Errorf("schedule: create once: %w", err)
	}

	return sch, nil
}

// List returns schedules, most recently created first. When desktopID is
// empty, schedules for every desktop are returned.
func (s *Store) List(desktopID string) []Schedule {
	query := `SELECT id, desktop_id, content, cron_expr, enabled, created_ns, last_run_ns, next_run_ns, run_once FROM schedules`

	args := []any{}

	if desktopID != "" {
		query += ` WHERE desktop_id = ?`
		args = append(args, desktopID)
	}

	query += ` ORDER BY created_ns DESC`

	schedules := make([]Schedule, 0)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		log.Printf("schedule: list: %v", err)
		return schedules
	}
	defer rows.Close()

	for rows.Next() {
		sch, err := scanSchedule(rows)
		if err != nil {
			log.Printf("schedule: list: %v", err)
			continue
		}

		schedules = append(schedules, sch)
	}

	return schedules
}

// Due returns enabled schedules whose next run has arrived by now.
func (s *Store) Due(now time.Time) ([]Schedule, error) {
	rows, err := s.db.Query(
		`SELECT id, desktop_id, content, cron_expr, enabled, created_ns, last_run_ns, next_run_ns, run_once
		 FROM schedules WHERE enabled = 1 AND next_run_ns > 0 AND next_run_ns <= ?`,
		sqlitedb.ToNS(now),
	)
	if err != nil {
		return nil, fmt.Errorf("schedule: due: %w", err)
	}
	defer rows.Close()

	due := make([]Schedule, 0)

	for rows.Next() {
		sch, err := scanSchedule(rows)
		if err != nil {
			return nil, fmt.Errorf("schedule: due: %w", err)
		}

		due = append(due, sch)
	}

	return due, rows.Err()
}

// MarkRun records that the schedule id fired at ranAt and advances it to its
// next occurrence, computed from ranAt rather than whatever "now" is by the
// time this runs. A cron expression that can no longer match anything (day 31
// in a month field restricted to February, say) disables the schedule
// instead of leaving it stuck re-checking a run that will never come. A
// one-time (RunOnce) schedule has no next occurrence by design - firing it
// once is the whole job - so it is simply disabled, the same terminal state a
// cron schedule reaches when it runs out of matches.
func (s *Store) MarkRun(id string, ranAt time.Time) error {
	var cronExpr string
	var runOnce int

	if err := s.db.QueryRow(`SELECT cron_expr, run_once FROM schedules WHERE id = ?`, id).Scan(&cronExpr, &runOnce); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}

		return fmt.Errorf("schedule: mark run %s: %w", id, err)
	}

	if runOnce != 0 {
		if _, err := s.db.Exec(
			`UPDATE schedules SET enabled = 0, last_run_ns = ?, next_run_ns = 0 WHERE id = ?`,
			sqlitedb.ToNS(ranAt), id,
		); err != nil {
			return fmt.Errorf("schedule: mark run %s: %w", id, err)
		}

		return nil
	}

	expr, err := Parse(cronExpr)
	if err != nil {
		return fmt.Errorf("schedule: mark run %s: %w", id, err)
	}

	next, ok := expr.Next(ranAt)

	if !ok {
		_, err = s.db.Exec(
			`UPDATE schedules SET enabled = 0, last_run_ns = ?, next_run_ns = 0 WHERE id = ?`,
			sqlitedb.ToNS(ranAt), id,
		)
	} else {
		_, err = s.db.Exec(
			`UPDATE schedules SET last_run_ns = ?, next_run_ns = ? WHERE id = ?`,
			sqlitedb.ToNS(ranAt), sqlitedb.ToNS(next), id,
		)
	}

	if err != nil {
		return fmt.Errorf("schedule: mark run %s: %w", id, err)
	}

	return nil
}

// SetEnabled turns a schedule on or off. Turning a cron schedule back on
// recomputes its next run from now, since whatever it last computed may be
// long past after sitting disabled. A one-time (RunOnce) schedule has no cron
// expression to recompute from, so re-enabling one instead keeps whatever
// run time it was created with - due immediately (a catch-up run, same as a
// cron schedule due after a control-plane outage) if that time has already
// passed while it sat paused - unless it already fired once, which is a
// terminal state MarkRun never returns from.
func (s *Store) SetEnabled(id string, enabled bool) error {
	if !enabled {
		if _, err := s.db.Exec(`UPDATE schedules SET enabled = 0 WHERE id = ?`, id); err != nil {
			return fmt.Errorf("schedule: disable %s: %w", id, err)
		}

		return nil
	}

	var cronExpr string
	var runOnce int
	var lastRunNS int64

	if err := s.db.QueryRow(`SELECT cron_expr, run_once, last_run_ns FROM schedules WHERE id = ?`, id).Scan(&cronExpr, &runOnce, &lastRunNS); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("schedule not found")
		}

		return fmt.Errorf("schedule: enable %s: %w", id, err)
	}

	if runOnce != 0 {
		if lastRunNS != 0 {
			return fmt.Errorf("this one-time run has already fired")
		}

		if _, err := s.db.Exec(`UPDATE schedules SET enabled = 1 WHERE id = ?`, id); err != nil {
			return fmt.Errorf("schedule: enable %s: %w", id, err)
		}

		return nil
	}

	expr, err := Parse(cronExpr)
	if err != nil {
		return fmt.Errorf("schedule: enable %s: %w", id, err)
	}

	next, ok := expr.Next(s.clock())
	if !ok {
		return fmt.Errorf("cron expression %q never matches", cronExpr)
	}

	if _, err := s.db.Exec(
		`UPDATE schedules SET enabled = 1, next_run_ns = ? WHERE id = ?`,
		sqlitedb.ToNS(next), id,
	); err != nil {
		return fmt.Errorf("schedule: enable %s: %w", id, err)
	}

	return nil
}

// Delete removes a schedule by ID. It is a no-op if the ID is not present.
func (s *Store) Delete(id string) {
	if _, err := s.db.Exec(`DELETE FROM schedules WHERE id = ?`, id); err != nil {
		log.Printf("schedule: delete %s: %v", id, err)
	}
}

// DeleteForDesktop removes every schedule targeting desktopID. Called when a
// desktop is deleted, so a schedule never outlives the desktop it fires
// requests at - left behind, it would keep matching forever: every
// occurrence, the submit fails fast against the now-nonexistent desktop and
// MarkRun just advances to the next one, an indefinite failure loop with
// nothing in the UI to say so (unlike a cron expression that can no longer
// match anything, which MarkRun already disables on its own).
func (s *Store) DeleteForDesktop(desktopID string) {
	if _, err := s.db.Exec(`DELETE FROM schedules WHERE desktop_id = ?`, desktopID); err != nil {
		log.Printf("schedule: delete for desktop %s: %v", desktopID, err)
	}
}

func scanSchedule(rows *sql.Rows) (Schedule, error) {
	var sch Schedule
	var enabled, runOnce int
	var created, lastRun, nextRun int64

	if err := rows.Scan(&sch.ID, &sch.DesktopID, &sch.Content, &sch.CronExpr, &enabled, &created, &lastRun, &nextRun, &runOnce); err != nil {
		return Schedule{}, err
	}

	sch.Enabled = enabled != 0
	sch.RunOnce = runOnce != 0
	sch.CreatedAt = sqlitedb.FromNS(created)
	sch.LastRunAt = sqlitedb.FromNS(lastRun)
	sch.NextRunAt = sqlitedb.FromNS(nextRun)

	return sch, nil
}

// newID generates a random schedule ID - not derived from the clock, so two
// schedules created within the same nanosecond (routine under a fixed test
// clock, if rare in production) never collide.
func newID() string {
	raw := make([]byte, 8)

	if _, err := rand.Read(raw); err != nil {
		panic(fmt.Sprintf("schedule: generating id: %v", err))
	}

	return "sched-" + hex.EncodeToString(raw)
}

func (s *Store) clock() time.Time {
	if s.now != nil {
		return s.now()
	}

	return time.Now().UTC()
}
