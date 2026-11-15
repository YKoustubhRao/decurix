// Package schedule stores recurring requests - the same content resubmitted
// to a desktop every time a cron expression next matches, the way a human
// retyping it and hitting Send would - and the Runner that fires them. A
// schedule is created from the requests page (see internal/httpapi's
// schedules.go), never from the chat: it is a fourth door onto the same
// Plan/deliver/Commit path the chat, the requests page and a query reply
// already share.
package schedule

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// Schedule is a recurring request, or a one-time request held for a single
// future run (RunOnce), which fires exactly once and then disables itself
// rather than computing a next occurrence.
type Schedule struct {
	ID        string `json:"id"`
	DesktopID string `json:"desktop_id"`
	Content   string `json:"content"`
	CronExpr  string `json:"cron_expr"`
	// Zone is the IANA time zone the cron is read in ("Asia/Kolkata"), and
	// the zone NextRunAt and LastRunAt are given in, so their JSON carries
	// its offset. Schedules made before zones existed read "UTC".
	Zone      string    `json:"zone"`
	RunOnce   bool      `json:"run_once"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	LastRunAt time.Time `json:"last_run_at"`
	NextRunAt time.Time `json:"next_run_at"`
	// Waiting says why a run that is due has not gone out yet (its desktop
	// is busy), and WaitingSince since when; both clear once it runs.
	Waiting      string    `json:"waiting"`
	WaitingSince time.Time `json:"waiting_since"`
	// LastError says why the last run did not reach its desktop; "" when it
	// did. A failed run is consumed (the schedule moves on to its next
	// occurrence) but never silently: this is its trace.
	LastError string `json:"last_error"`
	// RetryOccurrence is the run a failed try is being retried for (NextRunAt is then the retry's time); zero when
	// none. Missed says what the last stretch of occurrences that never ran was, until the next run settles.
	RetryOccurrence time.Time `json:"retry_occurrence"`
	Missed          string    `json:"missed"`
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
	`ALTER TABLE schedules ADD COLUMN waiting TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE schedules ADD COLUMN waiting_since_ns INTEGER NOT NULL DEFAULT 0`,
	// Why the last run did not reach its desktop (review 2026-11-03, B-28).
	`ALTER TABLE schedules ADD COLUMN last_error TEXT NOT NULL DEFAULT ''`,
	// The IANA zone the cron is read in (B-37). Existing rows keep UTC, the
	// zone they were made in, so nothing already scheduled moves.
	`ALTER TABLE schedules ADD COLUMN zone TEXT NOT NULL DEFAULT 'UTC'`,
	// The occurrence ledger (occurrences.go): one row per run a schedule owes.
	`CREATE TABLE schedule_runs (
		schedule_id   TEXT NOT NULL,
		occurrence_ns INTEGER NOT NULL,
		state         TEXT NOT NULL,
		started_ns    INTEGER NOT NULL,
		settled_ns    INTEGER NOT NULL DEFAULT 0,
		attempts      INTEGER NOT NULL DEFAULT 0,
		detail        TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (schedule_id, occurrence_ns)
	)`,
	`CREATE INDEX schedule_runs_recent ON schedule_runs (schedule_id, started_ns)`,
	// The occurrence a failed try is retried for (0: none), so the retry still says which run it is.
	`ALTER TABLE schedules ADD COLUMN retry_occurrence_ns INTEGER NOT NULL DEFAULT 0`,
	// What the last stretch of occurrences nobody ran was, shown beside the schedule until its next run settles.
	`ALTER TABLE schedules ADD COLUMN missed TEXT NOT NULL DEFAULT ''`,
}

// InputError is a mistake in what a person gave (a cron expression that does
// not parse, a time in the past): its text is written for that person and may
// be shown as it is. Every other error is internal and is not shown.
type InputError struct{ msg string }

// Error returns the message for the person.
func (e InputError) Error() string { return e.msg }

// invalid is an InputError.
func invalid(format string, args ...any) error { return InputError{fmt.Sprintf(format, args...)} }

// Store is a SQLite-backed collection of Schedules.
type Store struct {
	db *sql.DB

	// mu guards MarkRun and SetEnabled, the only two methods that read a
	// schedule's row and then write it back - with sqlitedb's pool no longer
	// held to one connection (see sqlitedb.Open), those two would otherwise
	// race each other's read and write on the same row: the Runner ticking a
	// schedule via MarkRun and a human toggling it via SetEnabled at the same
	// moment could interleave into a lost update. Every other method here is
	// one SQL statement, already atomic on its own.
	mu sync.Mutex

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

// Create validates cronExpr and zone (an IANA name; "" is DefaultZone) and
// persists a new schedule whose cron is read in that zone. Its first run is
// the next time the expression matches, never immediately - the same way
// cron itself never runs a job the instant it is added.
func (s *Store) Create(desktopID, content, cronExpr, zone string) (Schedule, error) {
	desktopID, err := checkRequest(desktopID, content)
	if err != nil {
		return Schedule{}, err
	}

	cronExpr = strings.TrimSpace(cronExpr)

	expr, err := Parse(cronExpr)
	if err != nil {
		return Schedule{}, invalid("%v", err)
	}

	zone, loc, err := LoadZone(zone)
	if err != nil {
		return Schedule{}, err
	}

	now := s.clock()

	next, ok := expr.NextIn(now, loc)
	if !ok {
		return Schedule{}, invalid("cron expression %q never matches", cronExpr)
	}

	return s.insert("create", Schedule{
		ID:        sqlitedb.NewID("sched-", 8),
		DesktopID: desktopID,
		Content:   content,
		CronExpr:  cronExpr,
		Zone:      zone,
		Enabled:   true,
		CreatedAt: now,
		NextRunAt: next,
	})
}

// CreateOnce persists a request held for a single future run at runAt, rather
// than a recurring cron expression. It fires exactly once (see MarkRun) - the
// same "never immediately" rule as Create applies here too: runAt must be
// strictly after now. runAt is an instant, so zone ("" is DefaultZone) only
// says how the run's TIME field and its times are given.
func (s *Store) CreateOnce(desktopID, content string, runAt time.Time, zone string) (Schedule, error) {
	desktopID, err := checkRequest(desktopID, content)
	if err != nil {
		return Schedule{}, err
	}

	zone, loc, err := LoadZone(zone)
	if err != nil {
		return Schedule{}, err
	}

	now := s.clock()
	runAt = runAt.UTC()

	if !runAt.After(now) {
		return Schedule{}, invalid("run time must be in the future")
	}

	return s.insert("create once", Schedule{
		ID:        sqlitedb.NewID("sched-", 8),
		DesktopID: desktopID,
		Content:   content,
		Zone:      zone,
		RunOnce:   true,
		Enabled:   true,
		CreatedAt: now,
		NextRunAt: runAt.In(loc),
	})
}

// checkRequest trims desktopID and checks that it and the request text are given.
func checkRequest(desktopID, content string) (string, error) {
	desktopID = strings.TrimSpace(desktopID)

	if desktopID == "" {
		return "", invalid("desktop id is required")
	}

	if strings.TrimSpace(content) == "" {
		return "", invalid("request text is required")
	}

	return desktopID, nil
}

// insert persists a new, enabled schedule that has never run; what names the
// caller in an error.
func (s *Store) insert(what string, sch Schedule) (Schedule, error) {
	if _, err := s.db.Exec(
		`INSERT INTO schedules (id, desktop_id, content, cron_expr, enabled, created_ns, last_run_ns, next_run_ns, run_once, zone)
		 VALUES (?, ?, ?, ?, 1, ?, 0, ?, ?, ?)`,
		sch.ID, sch.DesktopID, sch.Content, sch.CronExpr,
		sqlitedb.ToNS(sch.CreatedAt), sqlitedb.ToNS(sch.NextRunAt), sch.RunOnce, sch.Zone,
	); err != nil {
		return Schedule{}, fmt.Errorf("schedule: %s: %w", what, err)
	}

	return sch, nil
}

// List returns schedules, most recently created first. When desktopID is
// empty, schedules for every desktop are returned.
func (s *Store) List(desktopID string) []Schedule {
	query := `SELECT ` + scheduleColumns + ` FROM schedules`

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
	due, err := sqlitedb.QueryAll(s.db, scanSchedule,
		`SELECT `+scheduleColumns+` FROM schedules WHERE enabled = 1 AND next_run_ns > 0 AND next_run_ns <= ?`,
		sqlitedb.ToNS(now),
	)
	if err != nil {
		return nil, fmt.Errorf("schedule: due: %w", err)
	}

	return due, nil
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
	return s.markRun(id, ranAt, "")
}

// MarkRunFailed is MarkRun for a run that did not reach its desktop: the
// schedule moves on all the same, and keeps why as LastError.
func (s *Store) MarkRunFailed(id string, ranAt time.Time, why string) error {
	if why == "" {
		why = "the run did not reach its desktop"
	}
	return s.markRun(id, ranAt, why)
}

func (s *Store) markRun(id string, ranAt time.Time, failure string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return advance(s.db, id, ranAt, failure)
}

// execQuerier is what advance needs of a *sql.DB or a *sql.Tx.
type execQuerier interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

// advance moves schedule id on from ranAt: to its next occurrence, or disabled when there is none (a one-time run,
// or a cron that can no longer match). It clears what belonged to the run just made - waiting, a retry, a missed
// note - and keeps failure as LastError ("" when the run reached its desktop). The caller holds s.mu.
func advance(q execQuerier, id string, ranAt time.Time, failure string) error {
	var cronExpr, zone string
	var runOnce int

	if err := q.QueryRow(`SELECT cron_expr, run_once, zone FROM schedules WHERE id = ?`, id).Scan(&cronExpr, &runOnce, &zone); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}

		return fmt.Errorf("schedule: mark run %s: %w", id, err)
	}

	var next time.Time
	var ok bool

	if runOnce == 0 {
		expr, err := Parse(cronExpr)
		if err != nil {
			return fmt.Errorf("schedule: mark run %s: %w", id, err)
		}

		next, ok = expr.NextIn(ranAt, ZoneLocation(zone))
	}

	var err error

	if !ok {
		_, err = q.Exec(
			`UPDATE schedules SET enabled = 0, last_run_ns = ?, next_run_ns = 0, waiting = '', waiting_since_ns = 0, last_error = ?, retry_occurrence_ns = 0, missed = '' WHERE id = ?`,
			sqlitedb.ToNS(ranAt), failure, id,
		)
	} else {
		_, err = q.Exec(
			`UPDATE schedules SET last_run_ns = ?, next_run_ns = ?, waiting = '', waiting_since_ns = 0, last_error = ?, retry_occurrence_ns = 0, missed = '' WHERE id = ?`,
			sqlitedb.ToNS(ranAt), sqlitedb.ToNS(next), failure, id,
		)
	}

	if err != nil {
		return fmt.Errorf("schedule: mark run %s: %w", id, err)
	}

	return nil
}

// NoteWaiting records that a due run is held because its desktop is busy
// (why), keeping the time it first had to wait.
func (s *Store) NoteWaiting(id, why string, at time.Time) {
	if _, err := s.db.Exec(
		`UPDATE schedules SET waiting = ?, waiting_since_ns = CASE WHEN waiting_since_ns = 0 THEN ? ELSE waiting_since_ns END WHERE id = ?`,
		why, sqlitedb.ToNS(at), id,
	); err != nil {
		log.Printf("schedule: note waiting %s: %v", id, err)
	}
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
	s.mu.Lock()
	defer s.mu.Unlock()

	if !enabled {
		if _, err := s.db.Exec(`UPDATE schedules SET enabled = 0, waiting = '', waiting_since_ns = 0, retry_occurrence_ns = 0 WHERE id = ?`, id); err != nil {
			return fmt.Errorf("schedule: disable %s: %w", id, err)
		}

		// A run waiting for another try is given up with the schedule: turned back on, it starts from its next
		// occurrence, never from a retry a person paused.
		if _, err := s.db.Exec(`UPDATE schedule_runs SET state = ?, detail = detail || ' Given up: the schedule was turned off.' WHERE schedule_id = ? AND state = ?`,
			RunFailed, id, RunRetrying); err != nil {
			return fmt.Errorf("schedule: disable %s: %w", id, err)
		}

		return nil
	}

	var cronExpr, zone string
	var runOnce int
	var lastRunNS int64

	if err := s.db.QueryRow(`SELECT cron_expr, run_once, last_run_ns, zone FROM schedules WHERE id = ?`, id).Scan(&cronExpr, &runOnce, &lastRunNS, &zone); err != nil {
		if err == sql.ErrNoRows {
			return invalid("schedule not found")
		}

		return fmt.Errorf("schedule: enable %s: %w", id, err)
	}

	if runOnce != 0 {
		if lastRunNS != 0 {
			return invalid("this one-time run has already fired")
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

	next, ok := expr.NextIn(s.clock(), ZoneLocation(zone))
	if !ok {
		return invalid("cron expression %q never matches", cronExpr)
	}

	if _, err := s.db.Exec(
		`UPDATE schedules SET enabled = 1, next_run_ns = ?, retry_occurrence_ns = 0 WHERE id = ?`,
		sqlitedb.ToNS(next), id,
	); err != nil {
		return fmt.Errorf("schedule: enable %s: %w", id, err)
	}

	return nil
}

// Active reports whether any schedule is still waiting to fire - one of the
// things that should keep this machine from going to sleep (see
// internal/keepawake): a laptop asleep at 09:00 runs its 09:00 schedule only
// once it wakes.
func (s *Store) Active() bool {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM schedules WHERE enabled = 1 AND next_run_ns > 0`).Scan(&n); err != nil {
		return false
	}

	return n > 0
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

const scheduleColumns = `id, desktop_id, content, cron_expr, enabled, created_ns, last_run_ns, next_run_ns, run_once, waiting, waiting_since_ns, last_error, zone, retry_occurrence_ns, missed`

// scanSchedule reads one row of scheduleColumns, giving its run times in the
// schedule's own zone.
func scanSchedule(rows sqlitedb.Scanner) (Schedule, error) {
	var sch Schedule
	var enabled, runOnce int
	var created, lastRun, nextRun, waitingSince, retryOcc int64

	if err := rows.Scan(&sch.ID, &sch.DesktopID, &sch.Content, &sch.CronExpr, &enabled, &created, &lastRun, &nextRun, &runOnce, &sch.Waiting, &waitingSince, &sch.LastError, &sch.Zone, &retryOcc, &sch.Missed); err != nil {
		return Schedule{}, err
	}

	loc := ZoneLocation(sch.Zone)

	sch.Enabled = enabled != 0
	sch.RunOnce = runOnce != 0
	sch.CreatedAt = sqlitedb.FromNS(created)
	sch.LastRunAt = InZone(sqlitedb.FromNS(lastRun), loc)
	sch.NextRunAt = InZone(sqlitedb.FromNS(nextRun), loc)
	sch.WaitingSince = sqlitedb.FromNS(waitingSince)
	sch.RetryOccurrence = InZone(sqlitedb.FromNS(retryOcc), loc)

	return sch, nil
}

// InZone gives t in loc, leaving Go's zero time ("never") as it is.
func InZone(t time.Time, loc *time.Location) time.Time {
	if t.IsZero() {
		return t
	}

	return t.In(loc)
}

func (s *Store) clock() time.Time {
	if s.now != nil {
		return s.now()
	}

	return time.Now().UTC()
}
