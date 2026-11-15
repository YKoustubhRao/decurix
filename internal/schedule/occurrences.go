// Occurrences: one ledger row per run a schedule owes, so every occurrence is accounted for - sent once, retried
// while it never reached its desktop, given up with its reason, or missed while the control plane was not running.
//
// Before this, a crash between the send and MarkRun left the occurrence due, and the next start sent it a second
// time; a run whose desktop could not start was consumed (a monthly request lost until next month); and a stretch
// with the control plane down caught up with one run and said nothing of the others. Techniques from hermes-agent's
// cron (cron/occurrences.py, cron/executions.py, cron/missed_runs.py, cron/unreachable_retry.py): the slot is
// written down before dispatch, an occurrence never fires twice, a missed stretch is one visible row, and a run is
// retried only when it is known not to have started anything.
package schedule

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// Occurrence states.
const (
	// RunSending is an occurrence being sent. A row still in it at the next start is a send a crash cut short.
	RunSending = "sending"
	// RunSent reached its desktop.
	RunSent = "sent"
	// RunRetrying did not reach its desktop and is tried again (RetryAt).
	RunRetrying = "retrying"
	// RunFailed did not reach its desktop after every try; the schedule moved on.
	RunFailed = "failed"
	// RunUnconfirmed was cut short by a crash and its desktop's records cannot say whether it arrived: it is not
	// sent again (a second filing is worse than a missing one, and a person is told).
	RunUnconfirmed = "unconfirmed"
	// RunMissed stands for every occurrence that passed while the control plane was not running (Detail says how
	// many); only the latest was run.
	RunMissed = "missed"
)

// retryBackoff is how long after each failed try the occurrence is tried again; after the last, it is given up.
var retryBackoff = []time.Duration{5 * time.Minute, 15 * time.Minute, 30 * time.Minute}

// maxMissedCount bounds how far a missed stretch is counted (a minutely schedule after a month away).
const maxMissedCount = 10000

// Run is one occurrence's ledger row.
type Run struct {
	ScheduleID string    `json:"schedule_id"`
	Occurrence time.Time `json:"occurrence"`
	State      string    `json:"state"`
	StartedAt  time.Time `json:"started_at"`
	SettledAt  time.Time `json:"settled_at"`
	Attempts   int       `json:"attempts"`
	Detail     string    `json:"detail"`
}

// beginRun writes occurrence down as being sent, before anything is sent. It returns the row as it was when one
// already existed for this occurrence (a crash cut a send short, or a retry), with existed true.
func (s *Store) beginRun(id string, occurrence, now time.Time) (Run, bool, error) {
	prev, err := s.run(id, occurrence)
	if err == nil {
		if _, err := s.db.Exec(
			`UPDATE schedule_runs SET state = ?, started_ns = ? WHERE schedule_id = ? AND occurrence_ns = ? AND state = ?`,
			RunSending, sqlitedb.ToNS(now), id, sqlitedb.ToNS(occurrence), RunRetrying,
		); err != nil {
			return Run{}, false, fmt.Errorf("schedule: begin run %s: %w", id, err)
		}
		return prev, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Run{}, false, err
	}
	if _, err := s.db.Exec(
		`INSERT INTO schedule_runs (schedule_id, occurrence_ns, state, started_ns) VALUES (?, ?, ?, ?)`,
		id, sqlitedb.ToNS(occurrence), RunSending, sqlitedb.ToNS(now),
	); err != nil {
		return Run{}, false, fmt.Errorf("schedule: begin run %s: %w", id, err)
	}
	return Run{}, false, nil
}

// run reads one occurrence's row.
func (s *Store) run(id string, occurrence time.Time) (Run, error) {
	return scanRun(s.db.QueryRow(`SELECT `+runColumns+` FROM schedule_runs WHERE schedule_id = ? AND occurrence_ns = ?`,
		id, sqlitedb.ToNS(occurrence)))
}

// holdRun undoes beginRun for an occurrence that was not sent because its desktop was busy: it is begun again on a
// later tick. A retry goes back to retrying, keeping its tries; a first try's row is forgotten.
func (s *Store) holdRun(id string, occurrence time.Time, wasRetrying bool) {
	if wasRetrying {
		_, _ = s.db.Exec(`UPDATE schedule_runs SET state = ? WHERE schedule_id = ? AND occurrence_ns = ? AND state = ?`,
			RunRetrying, id, sqlitedb.ToNS(occurrence), RunSending)
		return
	}
	_, _ = s.db.Exec(`DELETE FROM schedule_runs WHERE schedule_id = ? AND occurrence_ns = ? AND state = ?`,
		id, sqlitedb.ToNS(occurrence), RunSending)
}

// settleRun records how an occurrence ended (sent, failed, unconfirmed) and, in the same transaction, advances the
// schedule from ranAt as MarkRun does, so a crash never leaves an occurrence settled and still due.
func (s *Store) settleRun(id string, occurrence, ranAt time.Time, state, detail string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("schedule: settle %s: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(
		`UPDATE schedule_runs SET state = ?, settled_ns = ?, attempts = attempts + 1, detail = ? WHERE schedule_id = ? AND occurrence_ns = ?`,
		state, sqlitedb.ToNS(ranAt), clip(detail), id, sqlitedb.ToNS(occurrence),
	); err != nil {
		return fmt.Errorf("schedule: settle %s: %w", id, err)
	}
	failure := ""
	if state != RunSent {
		failure = detail
	}
	if err := advance(tx, id, ranAt, failure); err != nil {
		return err
	}
	return tx.Commit()
}

// retryRun records a try that did not reach the desktop and holds the occurrence for another at retryAt: the
// schedule's next run becomes retryAt, and retry_occurrence_ns keeps which run it is.
func (s *Store) retryRun(id string, occurrence, now, retryAt time.Time, why string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("schedule: retry %s: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(
		`UPDATE schedule_runs SET state = ?, settled_ns = ?, attempts = attempts + 1, detail = ? WHERE schedule_id = ? AND occurrence_ns = ?`,
		RunRetrying, sqlitedb.ToNS(now), clip(why), id, sqlitedb.ToNS(occurrence),
	); err != nil {
		return fmt.Errorf("schedule: retry %s: %w", id, err)
	}
	if _, err := tx.Exec(
		`UPDATE schedules SET next_run_ns = ?, retry_occurrence_ns = ?, waiting = '', waiting_since_ns = 0, last_error = ? WHERE id = ?`,
		sqlitedb.ToNS(retryAt), sqlitedb.ToNS(occurrence), clip(why), id,
	); err != nil {
		return fmt.Errorf("schedule: retry %s: %w", id, err)
	}
	return tx.Commit()
}

// noteMissed records a stretch of occurrences that passed while nothing ran them: one ledger row for the first,
// saying how many, and the same words beside the schedule.
func (s *Store) noteMissed(id string, first, last time.Time, count int, now time.Time, loc *time.Location) {
	what := fmt.Sprintf("%d more run(s) came due before this one could be sent (%s to %s: the control plane was not "+
		"running, or the desktop stayed busy); they were not run separately.",
		count, first.In(loc).Format("2 Jan 2006 15:04 MST"), last.In(loc).Format("2 Jan 2006 15:04 MST"))
	if _, err := s.db.Exec(
		`INSERT OR IGNORE INTO schedule_runs (schedule_id, occurrence_ns, state, started_ns, settled_ns, detail) VALUES (?, ?, ?, ?, ?, ?)`,
		id, sqlitedb.ToNS(first), RunMissed, sqlitedb.ToNS(now), sqlitedb.ToNS(now), what,
	); err != nil {
		return
	}
	_, _ = s.db.Exec(`UPDATE schedules SET missed = ? WHERE id = ?`, what, id)
}

// Runs is a schedule's ledger, newest first, at most limit rows.
func (s *Store) Runs(id string, limit int) ([]Run, error) {
	if limit <= 0 {
		limit = 50
	}
	return sqlitedb.QueryAll(s.db, scanRun,
		`SELECT `+runColumns+` FROM schedule_runs WHERE schedule_id = ? ORDER BY occurrence_ns DESC LIMIT ?`, id, limit)
}

const runColumns = `schedule_id, occurrence_ns, state, started_ns, settled_ns, attempts, detail`

// scanRun reads one row of runColumns.
func scanRun(row sqlitedb.Scanner) (Run, error) {
	var r Run
	var occ, started, settled int64
	if err := row.Scan(&r.ScheduleID, &occ, &r.State, &started, &settled, &r.Attempts, &r.Detail); err != nil {
		return Run{}, err
	}
	r.Occurrence = sqlitedb.FromNS(occ)
	r.StartedAt = sqlitedb.FromNS(started)
	r.SettledAt = sqlitedb.FromNS(settled)
	return r, nil
}

// missedBetween counts the occurrences of a cron schedule strictly after occurrence and no later than now - the
// ones the run being made now stands in for - and gives the first and last of them.
func missedBetween(cronExpr string, occurrence, now time.Time, loc *time.Location) (first, last time.Time, count int) {
	expr, err := Parse(cronExpr)
	if err != nil {
		return
	}
	t := occurrence
	for count < maxMissedCount {
		next, ok := expr.NextIn(t, loc)
		if !ok || next.After(now) {
			break
		}
		if count == 0 {
			first = next
		}
		last = next
		count++
		t = next
	}
	return
}

// nextAfter is a cron schedule's first occurrence after t, or false when it has none (or is a one-time run).
func nextAfter(sch Schedule, t time.Time) (time.Time, bool) {
	if sch.RunOnce {
		return time.Time{}, false
	}
	expr, err := Parse(sch.CronExpr)
	if err != nil {
		return time.Time{}, false
	}
	return expr.NextIn(t, ZoneLocation(sch.Zone))
}
