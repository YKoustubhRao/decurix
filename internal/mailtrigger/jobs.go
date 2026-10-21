package mailtrigger

import (
	"fmt"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// Job statuses. A job is queued as pending, claimed as sending while it is
// handed to its desktop, and ends sent, failed or skipped; a person can
// cancel a pending job and retry any finished one.
const (
	JobPending   = "pending"
	JobSending   = "sending"
	JobSent      = "sent"
	JobFailed    = "failed"
	JobSkipped   = "skipped"
	JobCancelled = "cancelled"
)

// Job is one email that matched one rule.
type Job struct {
	ID        string    `json:"id"`
	RuleID    string    `json:"rule_id"`
	RuleName  string    `json:"rule_name"`
	AccountID string    `json:"account_id"`
	DesktopID string    `json:"desktop_id"`
	MessageID string    `json:"message_id"`
	From      string    `json:"from"`
	Subject   string    `json:"subject"`
	Received  time.Time `json:"received"`
	Content   string    `json:"content"`
	Status    string    `json:"status"`
	// Files are the email's attachments, set when the poll makes the job and
	// kept in mail_job_files until it is sent; not read back into lists.
	Files []Attachment `json:"-"`
	// Note says why a job is waiting, failed or was skipped.
	Note   string `json:"note"`
	TaskID string `json:"task_id"`
	// Attempts counts the sends that failed; NextAttemptAt is when a job
	// held back after one may be tried again.
	Attempts      int       `json:"attempts"`
	NextAttemptAt time.Time `json:"next_attempt_at"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// maxJobs is how many finished jobs are kept for the history; waiting ones
// are never pruned.
const maxJobs = 500

const jobColumns = `id, rule_id, rule_name, account_id, desktop_id, message_id, sender, subject, received_ns, content,
	status, note, task_id, attempts, next_attempt_ns, created_ns, updated_ns`

// Jobs returns the most recent jobs, newest first.
func (s *Store) Jobs(limit int) ([]Job, error) {
	if limit <= 0 || limit > maxJobs {
		limit = 100
	}

	return s.queryJobs(`SELECT `+jobColumns+` FROM mail_jobs ORDER BY created_ns DESC, id LIMIT ?`, limit)
}

// Pending returns the jobs waiting to be sent, oldest first - the order the
// emails arrived in, which is the order a desktop does them.
func (s *Store) Pending() ([]Job, error) {
	return s.queryJobs(`SELECT ` + jobColumns + ` FROM mail_jobs WHERE status = 'pending' ORDER BY created_ns, received_ns, id`)
}

// Waiting reports whether any job is still to be sent - one of the things
// that should keep this machine awake (see internal/keepawake).
func (s *Store) Waiting() bool {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM mail_jobs WHERE status IN ('pending', 'sending')`).Scan(&n); err != nil {
		return false
	}

	return n > 0
}

func (s *Store) queryJobs(query string, args ...any) ([]Job, error) {
	jobs, err := sqlitedb.QueryAll(s.db, func(r sqlitedb.Scanner) (Job, error) {
		var j Job
		var received, nextAttempt, created, updated int64

		if err := r.Scan(&j.ID, &j.RuleID, &j.RuleName, &j.AccountID, &j.DesktopID, &j.MessageID, &j.From, &j.Subject,
			&received, &j.Content, &j.Status, &j.Note, &j.TaskID, &j.Attempts, &nextAttempt, &created, &updated); err != nil {
			return Job{}, err
		}

		j.Received = sqlitedb.FromNS(received)
		j.NextAttemptAt = sqlitedb.FromNS(nextAttempt)
		j.CreatedAt = sqlitedb.FromNS(created)
		j.UpdatedAt = sqlitedb.FromNS(updated)

		return j, nil
	}, query, args...)
	if err != nil {
		return nil, fmt.Errorf("mailtrigger: jobs: %w", err)
	}

	return jobs, nil
}

// claimJob moves a pending job that is due to sending, and reports whether
// this caller got it: of two dispatchers racing for one job, exactly one
// sends it.
func (s *Store) claimJob(id string, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.Exec(
		`UPDATE mail_jobs SET status = 'sending', updated_ns = ? WHERE id = ? AND status = 'pending' AND next_attempt_ns <= ?`,
		sqlitedb.ToNS(now), id, sqlitedb.ToNS(now),
	)
	if err != nil {
		return false, fmt.Errorf("mailtrigger: claim job: %w", err)
	}

	n, err := res.RowsAffected()

	return n == 1, err
}

// releaseJob puts a claimed job back to waiting without counting an attempt
// (its desktop was busy), with why.
func (s *Store) releaseJob(id, note string) error {
	return s.fromSending(id, JobPending, note, "", 0, time.Time{})
}

// retryJobLater puts a claimed job back to waiting after a failed send, not
// to be tried again before next.
func (s *Store) retryJobLater(id, note string, next time.Time) error {
	return s.fromSending(id, JobPending, note, "", 1, next)
}

// finishJob ends a claimed job.
func (s *Store) finishJob(id, status, note, taskID string, failedAttempt bool) error {
	return s.fromSending(id, status, note, taskID, boolInt(failedAttempt), time.Time{})
}

func (s *Store) fromSending(id, status, note, taskID string, addAttempts int, next time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.db.Exec(
		`UPDATE mail_jobs SET status = ?, note = ?, task_id = ?, attempts = attempts + ?, next_attempt_ns = ?, updated_ns = ?
		 WHERE id = ? AND status = 'sending'`,
		status, note, taskID, addAttempts, sqlitedb.ToNS(next), sqlitedb.ToNS(s.clock()), id,
	); err != nil {
		return fmt.Errorf("mailtrigger: update job: %w", err)
	}

	s.pruneLocked()

	return nil
}

// noteWaiting records why a pending job has not been sent yet.
func (s *Store) noteWaiting(id, note string) {
	_, err := s.db.Exec(`UPDATE mail_jobs SET note = ? WHERE id = ? AND status = 'pending' AND note != ?`, note, id, note)
	logErr(err)
}

// RecoverInterrupted ends every job that was being sent when the control
// plane last stopped. Whether its desktop got it is unknown - the task may
// have run - so it is failed with a note for a person to check, never sent
// again on its own. Called once, before the Runner starts.
func (s *Store) RecoverInterrupted() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.Exec(
		`UPDATE mail_jobs SET status = 'failed', updated_ns = ?,
		 note = 'Decurix stopped while this was being sent, so it may or may not have reached the desktop. Check the desktop''s chat; Run again only if it is not there.'
		 WHERE status = 'sending'`, sqlitedb.ToNS(s.clock()),
	)
	if err != nil {
		return 0, fmt.Errorf("mailtrigger: recover jobs: %w", err)
	}

	n, err := res.RowsAffected()

	return int(n), err
}

// CancelJob stops a waiting job from being sent. One already being sent
// cannot be stopped.
func (s *Store) CancelJob(id string) error {
	return s.moveJob(id, JobCancelled, "Cancelled", `status = 'pending'`)
}

// RetryJob queues a finished job again, to be sent the next time its desktop
// is free. Its request is sent exactly as it was queued.
func (s *Store) RetryJob(id string) error {
	return s.moveJob(id, JobPending, "", `status IN ('failed', 'skipped', 'cancelled', 'sent')`)
}

func (s *Store) moveJob(id, status, note, where string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.Exec(`UPDATE mail_jobs SET status = ?, note = ?, task_id = '', attempts = 0, next_attempt_ns = 0, updated_ns = ?
		WHERE id = ? AND `+where,
		status, note, sqlitedb.ToNS(s.clock()), id)
	if err != nil {
		return fmt.Errorf("mailtrigger: update job: %w", err)
	}

	return mustAffect(res)
}

// pruneLocked keeps the history to maxJobs finished jobs.
func (s *Store) pruneLocked() {
	_, err := s.db.Exec(
		`DELETE FROM mail_jobs WHERE status NOT IN ('pending', 'sending') AND id NOT IN (
			SELECT id FROM mail_jobs WHERE status NOT IN ('pending', 'sending') ORDER BY created_ns DESC LIMIT ?)`, maxJobs,
	)
	logErr(err)
}
