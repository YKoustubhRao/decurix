// Package mailtrigger starts a desktop's task when an email arrives: a
// mailbox connection (Account) is polled over IMAP, each new email is matched
// against the rules set up for that mailbox (a regular expression on its
// sender and/or subject), and a match queues a job - the rule's request, with
// the email attached - that is sent to the rule's desktop through the same
// door every other automated request takes (Submitter, satisfied by
// *httpapi.Server's SubmitTracked).
//
// It is built for a laptop that sleeps. A poll asks the server for every
// email above the last UID it saw, so mail that arrived while the machine
// slept is found on the first poll after it wakes, and nothing is lost or
// queued twice across a sleep, a restart or a dropped connection. Jobs are
// queued in SQLite before they are sent, and a job for a busy desktop waits
// instead of being sent: a request reaching an agent that is waiting on a
// person's answer is taken as that answer (see tasks.Plan), and an email's
// text must never become someone's "yes" to a payment.
//
// Sending is at most once. A job is claimed before it is sent, so two
// dispatchers cannot both send it; one that was being sent when the control
// plane stopped is marked for a person to check, never sent again on its own
// - the task behind it may already have run, and may have consequences.
//
// Depends on nothing internal but sqlitedb.
package mailtrigger

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// Account is one mailbox connection: where to log in, and how far the poll
// has read. Its password is never serialised - nothing outside this package
// can read it back, the page included - and is encrypted in the database
// (secret.go).
type Account struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Host      string    `json:"host"`
	Port      int       `json:"port"`
	Folder    string    `json:"folder"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	// LastCheckAt is the last poll attempt, LastOKAt the last one that
	// worked, and LastError what went wrong with the latest one ("" when it
	// worked). Failures counts the failed polls in a row, which is what
	// spaces the next ones out.
	LastCheckAt time.Time `json:"last_check_at"`
	LastOKAt    time.Time `json:"last_ok_at"`
	LastError   string    `json:"last_error"`
	Failures    int       `json:"failures"`
	// NeedsPassword says the latest error was the server refusing the
	// password (or a saved password that can no longer be decrypted), which
	// only typing it again fixes. Such a mailbox is not polled until then:
	// logging in again and again with a refused password is how an account
	// gets locked.
	NeedsPassword bool `json:"needs_password"`
	// Status is what the page shows: "paused", "connecting" (never polled
	// yet), "needs_password", "error" or "connected".
	Status string `json:"status"`
	// Seen counts the emails the poll has read since the mailbox was
	// connected; emails already there when it was connected are not counted
	// and never trigger anything.
	Seen int `json:"seen"`

	password    string
	passwordErr error
	uidValidity uint32
	lastUID     uint32
}

// ErrNotFound is an account, rule or job ID that does not exist (or, for a
// job, is not in a state the change applies to).
var ErrNotFound = errors.New("not found")

var schema = []string{
	`CREATE TABLE mail_accounts (
		id             TEXT PRIMARY KEY,
		email          TEXT NOT NULL,
		password       TEXT NOT NULL,
		host           TEXT NOT NULL,
		port           INTEGER NOT NULL,
		folder         TEXT NOT NULL,
		enabled        INTEGER NOT NULL,
		created_ns     INTEGER NOT NULL,
		uid_validity   INTEGER NOT NULL DEFAULT 0,
		last_uid       INTEGER NOT NULL DEFAULT 0,
		last_check_ns  INTEGER NOT NULL DEFAULT 0,
		last_ok_ns     INTEGER NOT NULL DEFAULT 0,
		last_error     TEXT NOT NULL DEFAULT '',
		needs_password INTEGER NOT NULL DEFAULT 0,
		failures       INTEGER NOT NULL DEFAULT 0,
		seen           INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE TABLE mail_rules (
		id            TEXT PRIMARY KEY,
		account_id    TEXT NOT NULL,
		desktop_id    TEXT NOT NULL,
		name          TEXT NOT NULL,
		sender_re     TEXT NOT NULL,
		subject_re    TEXT NOT NULL,
		content       TEXT NOT NULL,
		include_body  INTEGER NOT NULL,
		verified_only INTEGER NOT NULL,
		enabled       INTEGER NOT NULL,
		created_ns    INTEGER NOT NULL,
		last_fired_ns INTEGER NOT NULL DEFAULT 0,
		fired         INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE TABLE mail_jobs (
		id              TEXT PRIMARY KEY,
		rule_id         TEXT NOT NULL,
		rule_name       TEXT NOT NULL,
		account_id      TEXT NOT NULL,
		desktop_id      TEXT NOT NULL,
		message_id      TEXT NOT NULL,
		sender          TEXT NOT NULL,
		subject         TEXT NOT NULL,
		received_ns     INTEGER NOT NULL,
		content         TEXT NOT NULL,
		status          TEXT NOT NULL,
		note            TEXT NOT NULL DEFAULT '',
		task_id         TEXT NOT NULL DEFAULT '',
		attempts        INTEGER NOT NULL DEFAULT 0,
		next_attempt_ns INTEGER NOT NULL DEFAULT 0,
		created_ns      INTEGER NOT NULL,
		updated_ns      INTEGER NOT NULL
	)`,
	`CREATE INDEX mail_jobs_status ON mail_jobs (status, created_ns)`,
	`CREATE INDEX mail_rules_account ON mail_rules (account_id)`,
	// The files a matching email carried, kept until its job is sent.
	`CREATE TABLE mail_job_files (
		job_id       TEXT NOT NULL,
		position     INTEGER NOT NULL,
		name         TEXT NOT NULL,
		content_type TEXT NOT NULL,
		data         BLOB NOT NULL,
		PRIMARY KEY (job_id, position)
	)`,
}

// Store is the SQLite-backed collection of accounts, rules and jobs.
type Store struct {
	db  *sql.DB
	key []byte

	// mu guards every read-modify-write below (recording a poll, moving a
	// job between states) - see sqlitedb.Open for why the pool no longer
	// serialises them on its own.
	mu sync.Mutex

	now func() time.Time
}

// NewStore opens the Store on db, creating its tables if needed. key
// (LoadKey) encrypts mailbox passwords; nil stores them as they are. A
// password saved before there was a key is encrypted now.
func NewStore(db *sql.DB, key []byte) (*Store, error) {
	if err := sqlitedb.Migrate(db, "mailtrigger", schema); err != nil {
		return nil, err
	}

	s := &Store{db: db, key: key}

	if key != nil {
		if err := s.sealPlainPasswords(); err != nil {
			return nil, err
		}
	}

	return s, nil
}

func (s *Store) sealPlainPasswords() error {
	rows, err := s.db.Query(`SELECT id, password FROM mail_accounts WHERE password NOT LIKE ?`, sealedPrefix+"%")
	if err != nil {
		return fmt.Errorf("mailtrigger: reading passwords: %w", err)
	}

	plain := map[string]string{}
	for rows.Next() {
		var id, password string
		if err := rows.Scan(&id, &password); err != nil {
			rows.Close()
			return fmt.Errorf("mailtrigger: reading passwords: %w", err)
		}

		plain[id] = password
	}
	rows.Close()

	for id, password := range plain {
		sealed, err := seal(s.key, password)
		if err != nil {
			return err
		}

		if _, err := s.db.Exec(`UPDATE mail_accounts SET password = ? WHERE id = ?`, sealed, id); err != nil {
			return fmt.Errorf("mailtrigger: encrypting a password: %w", err)
		}
	}

	return nil
}

// NewAccount is what a person gives to connect a mailbox.
type NewAccount struct {
	Email    string
	Password string
	Host     string
	Port     int
	Folder   string
}

// Normalise fills in Gmail's defaults and validates the rest.
func (n *NewAccount) Normalise() error {
	n.Email = strings.TrimSpace(n.Email)
	n.Host = strings.ToLower(strings.TrimSpace(n.Host))
	n.Folder = strings.TrimSpace(n.Folder)

	if !strings.Contains(n.Email, "@") || len(n.Email) > 320 {
		return fmt.Errorf("enter the email address of the mailbox")
	}

	if n.Host == "" {
		n.Host = "imap.gmail.com"
	}

	if n.Port == 0 {
		n.Port = 993
	}

	if n.Port < 1 || n.Port > 65535 {
		return fmt.Errorf("the port must be between 1 and 65535")
	}

	if n.Folder == "" {
		n.Folder = "INBOX"
	}

	n.Password = normalisePassword(n.Host, n.Password)

	if n.Password == "" {
		return fmt.Errorf("enter the mailbox's app password")
	}

	for _, v := range []string{n.Email, n.Password, n.Host, n.Folder} {
		if strings.ContainsAny(v, "\r\n\x00") {
			return fmt.Errorf("a line break is not allowed in the connection details")
		}
	}

	return nil
}

// normalisePassword drops the spaces Google shows an app password with (four
// groups of four); they are not part of it.
func normalisePassword(host, password string) string {
	if host == "imap.gmail.com" {
		return strings.ReplaceAll(password, " ", "")
	}

	return password
}

// WithPassword is a copy of a saved connection with another password, for
// testing it before UpdatePassword saves it.
func (a Account) WithPassword(password string) (Account, string, error) {
	password = normalisePassword(a.Host, password)
	if password == "" || strings.ContainsAny(password, "\r\n\x00") {
		return Account{}, "", fmt.Errorf("enter the mailbox's app password")
	}

	a.password = password
	a.passwordErr = nil

	return a, password, nil
}

// Account is the unsaved Account n describes, for a connection test.
func (n NewAccount) Account() Account {
	return Account{Email: n.Email, Host: n.Host, Port: n.Port, Folder: n.Folder, Enabled: true, password: n.Password}
}

// CreateAccount saves a connection that has already been tested, with the
// mailbox position the test read (uidValidity, uidNext): only email arriving
// after that is ever matched, so connecting a mailbox never fires on the
// thousands of messages already in it.
func (s *Store) CreateAccount(n NewAccount, uidValidity, uidNext uint32) (Account, error) {
	sealed, err := seal(s.key, n.Password)
	if err != nil {
		return Account{}, fmt.Errorf("mailtrigger: encrypting the password: %w", err)
	}

	now := s.clock()

	acct := n.Account()
	acct.ID = newID("mbox")
	acct.CreatedAt = now
	acct.LastCheckAt = now
	acct.LastOKAt = now
	acct.uidValidity = uidValidity
	acct.lastUID = lastBefore(uidNext)

	if _, err := s.db.Exec(
		`INSERT INTO mail_accounts (id, email, password, host, port, folder, enabled, created_ns, uid_validity, last_uid, last_check_ns, last_ok_ns)
		 VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?)`,
		acct.ID, acct.Email, sealed, acct.Host, acct.Port, acct.Folder, sqlitedb.ToNS(now),
		acct.uidValidity, acct.lastUID, sqlitedb.ToNS(now), sqlitedb.ToNS(now),
	); err != nil {
		return Account{}, fmt.Errorf("mailtrigger: create account: %w", err)
	}

	acct.Status = status(acct)

	return acct, nil
}

func lastBefore(uidNext uint32) uint32 {
	if uidNext == 0 {
		return 0
	}

	return uidNext - 1
}

// UpdatePassword replaces a connection's password (after a test with it
// worked) and clears the error that made it necessary, so polling resumes.
func (s *Store) UpdatePassword(id, password string) error {
	sealed, err := seal(s.key, password)
	if err != nil {
		return fmt.Errorf("mailtrigger: encrypting the password: %w", err)
	}

	res, err := s.db.Exec(`UPDATE mail_accounts SET password = ?, last_error = '', needs_password = 0, failures = 0 WHERE id = ?`, sealed, id)
	if err != nil {
		return fmt.Errorf("mailtrigger: update password: %w", err)
	}

	return mustAffect(res)
}

// SetAccountEnabled pauses or resumes polling a mailbox. A paused mailbox
// keeps its place: resuming it picks up everything that arrived meanwhile,
// the same as waking from sleep.
func (s *Store) SetAccountEnabled(id string, enabled bool) error {
	res, err := s.db.Exec(`UPDATE mail_accounts SET enabled = ?, failures = 0 WHERE id = ?`, boolInt(enabled), id)
	if err != nil {
		return fmt.Errorf("mailtrigger: enable account: %w", err)
	}

	return mustAffect(res)
}

// DeleteAccount disconnects a mailbox: its saved password, its rules and its
// waiting jobs go with it. Finished jobs stay in the history.
func (s *Store) DeleteAccount(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("mailtrigger: delete account: %w", err)
	}
	defer tx.Rollback()

	for _, q := range []string{
		`DELETE FROM mail_accounts WHERE id = ?`,
		`DELETE FROM mail_rules WHERE account_id = ?`,
		`DELETE FROM mail_jobs WHERE account_id = ? AND status = 'pending'`,
	} {
		if _, err := tx.Exec(q, id); err != nil {
			return fmt.Errorf("mailtrigger: delete account: %w", err)
		}
	}

	return tx.Commit()
}

const accountColumns = `id, email, password, host, port, folder, enabled, created_ns, uid_validity, last_uid,
	last_check_ns, last_ok_ns, last_error, needs_password, failures, seen`

// Accounts returns every mailbox connection, oldest first.
func (s *Store) Accounts() ([]Account, error) {
	rows, err := s.db.Query(`SELECT ` + accountColumns + ` FROM mail_accounts ORDER BY created_ns`)
	if err != nil {
		return nil, fmt.Errorf("mailtrigger: accounts: %w", err)
	}
	defer rows.Close()

	accounts := make([]Account, 0)

	for rows.Next() {
		acct, err := s.scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("mailtrigger: accounts: %w", err)
		}

		accounts = append(accounts, acct)
	}

	return accounts, rows.Err()
}

// Account returns one mailbox connection.
func (s *Store) Account(id string) (Account, error) {
	rows, err := s.db.Query(`SELECT `+accountColumns+` FROM mail_accounts WHERE id = ?`, id)
	if err != nil {
		return Account{}, fmt.Errorf("mailtrigger: account: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Account{}, err
		}

		return Account{}, ErrNotFound
	}

	return s.scanAccount(rows)
}

func (s *Store) scanAccount(rows *sql.Rows) (Account, error) {
	var a Account
	var enabled, needsPassword int
	var created, lastCheck, lastOK int64
	var validity, lastUID int64
	var stored string

	if err := rows.Scan(&a.ID, &a.Email, &stored, &a.Host, &a.Port, &a.Folder, &enabled, &created,
		&validity, &lastUID, &lastCheck, &lastOK, &a.LastError, &needsPassword, &a.Failures, &a.Seen); err != nil {
		return Account{}, err
	}

	a.password, a.passwordErr = unseal(s.key, stored)
	a.Enabled = enabled != 0
	a.NeedsPassword = needsPassword != 0 || a.passwordErr != nil
	a.CreatedAt = sqlitedb.FromNS(created)
	a.LastCheckAt = sqlitedb.FromNS(lastCheck)
	a.LastOKAt = sqlitedb.FromNS(lastOK)
	a.uidValidity = uint32(validity)
	a.lastUID = uint32(lastUID)

	if a.passwordErr != nil && a.LastError == "" {
		a.LastError = a.passwordErr.Error()
	}

	a.Status = status(a)

	return a, nil
}

func status(a Account) string {
	switch {
	case !a.Enabled:
		return "paused"
	case a.NeedsPassword:
		return "needs_password"
	case a.LastError != "":
		return "error"
	case a.LastOKAt.IsZero():
		return "connecting"
	default:
		return "connected"
	}
}

// Poll is what one poll of a mailbox found, recorded by RecordPoll.
type Poll struct {
	At          time.Time
	Err         error
	UIDValidity uint32
	// LastUID is the highest UID read; everything up to it is done with.
	LastUID uint32
	Seen    int
	Jobs    []Job
}

// RecordPoll saves a poll's result: its new jobs and the mailbox's new
// position in one transaction, so a crash between the two can neither lose
// an email (position saved, job not) nor queue it twice (job saved, position
// not). A poll of a mailbox deleted while it ran is dropped, and a job for a
// rule deleted or paused while it ran is not queued.
func (s *Store) RecordPoll(accountID string, p Poll) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("mailtrigger: record poll: %w", err)
	}
	defer tx.Rollback()

	// A poll that failed part way still saves how far it got (and the jobs
	// it queued up to there): Poll starts from the account's own position,
	// so one that failed before reading anything leaves it where it was.
	res, err := tx.Exec(`UPDATE mail_accounts SET last_check_ns = ?, uid_validity = ?, last_uid = ?, seen = seen + ? WHERE id = ?`,
		sqlitedb.ToNS(p.At), p.UIDValidity, p.LastUID, p.Seen, accountID)
	if err != nil {
		return fmt.Errorf("mailtrigger: record poll: %w", err)
	}

	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}

	if p.Err != nil {
		_, err = tx.Exec(`UPDATE mail_accounts SET last_error = ?, needs_password = ?, failures = failures + 1 WHERE id = ?`,
			clip(p.Err.Error()), boolInt(NeedsPassword(p.Err)), accountID)
	} else {
		_, err = tx.Exec(`UPDATE mail_accounts SET last_ok_ns = ?, last_error = '', needs_password = 0, failures = 0 WHERE id = ?`,
			sqlitedb.ToNS(p.At), accountID)
	}

	if err != nil {
		return fmt.Errorf("mailtrigger: record poll: %w", err)
	}

	for _, job := range p.Jobs {
		res, err := tx.Exec(
			`INSERT INTO mail_jobs (id, rule_id, rule_name, account_id, desktop_id, message_id, sender, subject, received_ns, content, status, note, created_ns, updated_ns)
			 SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
			 WHERE EXISTS (SELECT 1 FROM mail_rules WHERE id = ? AND enabled = 1)`,
			job.ID, job.RuleID, job.RuleName, job.AccountID, job.DesktopID, job.MessageID, job.From, job.Subject,
			sqlitedb.ToNS(job.Received), job.Content, job.Status, job.Note, sqlitedb.ToNS(p.At), sqlitedb.ToNS(p.At),
			job.RuleID,
		)
		if err != nil {
			return fmt.Errorf("mailtrigger: record poll: %w", err)
		}

		if n, _ := res.RowsAffected(); n == 1 && len(job.Files) > 0 {
			if err := insertJobFiles(tx, job.ID, job.Files); err != nil {
				return err
			}
		}

		if n, _ := res.RowsAffected(); n == 1 && job.Status == JobPending {
			if _, err := tx.Exec(`UPDATE mail_rules SET last_fired_ns = ?, fired = fired + 1 WHERE id = ?`,
				sqlitedb.ToNS(p.At), job.RuleID); err != nil {
				return fmt.Errorf("mailtrigger: record poll: %w", err)
			}
		}
	}

	return tx.Commit()
}

// Active reports whether any mailbox is being polled - one of the things
// that should keep this machine from going to sleep (see internal/keepawake).
// One waiting for a new password is not polled, so it does not count.
func (s *Store) Active() bool {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM mail_accounts WHERE enabled = 1 AND needs_password = 0`).Scan(&n); err != nil {
		return false
	}

	return n > 0
}

// NeedsPassword reports whether err is one only a new password fixes: the
// server refusing the login, or a saved password that cannot be decrypted.
func NeedsPassword(err error) bool {
	return errors.Is(err, ErrAuth) || errors.Is(err, ErrPasswordUnreadable)
}

func mustAffect(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}

	if n == 0 {
		return ErrNotFound
	}

	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}

	return 0
}

func newID(prefix string) string {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		panic(fmt.Sprintf("mailtrigger: generating id: %v", err))
	}

	return prefix + "-" + hex.EncodeToString(raw)
}

func (s *Store) clock() time.Time {
	if s.now != nil {
		return s.now()
	}

	return time.Now().UTC()
}

func logErr(err error) {
	if err != nil {
		log.Printf("mailtrigger: %v", err)
	}
}
