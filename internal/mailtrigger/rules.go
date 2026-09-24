package mailtrigger

import (
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// Rule turns matching email into a request for a desktop. Every pattern that
// is set must match (a rule with both is "this sender AND this subject"); at
// least one must be set. Patterns are Go regular expressions, matched
// case-insensitively; the sender pattern is tried against the address alone
// and against the whole From line, so both `@acme\.com$` and `^Billing` work.
type Rule struct {
	ID        string `json:"id"`
	AccountID string `json:"account_id"`
	DesktopID string `json:"desktop_id"`
	Name      string `json:"name"`
	SenderRe  string `json:"sender_re"`
	SubjectRe string `json:"subject_re"`
	// Content is the request sent to the desktop. {subject}, {from},
	// {sender} and {date} are replaced with the email's; see Render for what
	// is appended to it.
	Content string `json:"content"`
	// IncludeBody appends the email's text to the request.
	IncludeBody bool `json:"include_body"`
	// VerifiedOnly fires only for an email the provider confirmed comes from
	// its sender's domain (Message.Verified); others are recorded as skipped.
	VerifiedOnly bool      `json:"verified_only"`
	Enabled      bool      `json:"enabled"`
	CreatedAt    time.Time `json:"created_at"`
	LastFiredAt  time.Time `json:"last_fired_at"`
	Fired        int       `json:"fired"`
}

// maxPattern bounds a pattern's length. Go's regexp engine runs in linear
// time whatever the pattern, so this is about keeping a rule readable, not
// about a pattern being able to hang the poll.
const maxPattern = 500

// NewRule is what a person gives to set up a rule.
type NewRule struct {
	AccountID    string
	DesktopID    string
	Name         string
	SenderRe     string
	SubjectRe    string
	Content      string
	IncludeBody  bool
	VerifiedOnly bool
}

// CompilePattern compiles a rule pattern the way matching uses it
// (case-insensitive). An empty pattern is nil: it matches everything.
func CompilePattern(pattern string) (*regexp.Regexp, error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return nil, nil
	}

	if len(pattern) > maxPattern {
		return nil, fmt.Errorf("a pattern can be at most %d characters", maxPattern)
	}

	re, err := regexp.Compile("(?i)" + pattern)
	if err != nil {
		return nil, fmt.Errorf("%q is not a valid regular expression: %v", pattern, err)
	}

	return re, nil
}

// CreateRule validates and saves a rule.
func (s *Store) CreateRule(n NewRule) (Rule, error) {
	n.Name = strings.TrimSpace(n.Name)
	n.SenderRe = strings.TrimSpace(n.SenderRe)
	n.SubjectRe = strings.TrimSpace(n.SubjectRe)

	if n.DesktopID == "" {
		return Rule{}, fmt.Errorf("choose the desktop that should do the work")
	}

	if strings.TrimSpace(n.Content) == "" {
		return Rule{}, fmt.Errorf("write the request the desktop should get")
	}

	if n.SenderRe == "" && n.SubjectRe == "" {
		return Rule{}, fmt.Errorf("give a sender pattern, a subject pattern, or both")
	}

	for _, p := range []string{n.SenderRe, n.SubjectRe} {
		if _, err := CompilePattern(p); err != nil {
			return Rule{}, err
		}
	}

	if len(n.Name) > 200 {
		return Rule{}, fmt.Errorf("a name can be at most 200 characters")
	}

	if _, err := s.Account(n.AccountID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return Rule{}, fmt.Errorf("choose a connected mailbox")
		}

		return Rule{}, err
	}

	if n.Name == "" {
		n.Name = describe(n.SenderRe, n.SubjectRe)
	}

	now := s.clock()

	rule := Rule{
		ID:           newID("rule"),
		AccountID:    n.AccountID,
		DesktopID:    n.DesktopID,
		Name:         n.Name,
		SenderRe:     n.SenderRe,
		SubjectRe:    n.SubjectRe,
		Content:      n.Content,
		IncludeBody:  n.IncludeBody,
		VerifiedOnly: n.VerifiedOnly,
		Enabled:      true,
		CreatedAt:    now,
	}

	if _, err := s.db.Exec(
		`INSERT INTO mail_rules (id, account_id, desktop_id, name, sender_re, subject_re, content, include_body, verified_only, enabled, created_ns)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)`,
		rule.ID, rule.AccountID, rule.DesktopID, rule.Name, rule.SenderRe, rule.SubjectRe, rule.Content,
		boolInt(rule.IncludeBody), boolInt(rule.VerifiedOnly), sqlitedb.ToNS(now),
	); err != nil {
		return Rule{}, fmt.Errorf("mailtrigger: create rule: %w", err)
	}

	return rule, nil
}

func describe(senderRe, subjectRe string) string {
	switch {
	case senderRe != "" && subjectRe != "":
		return "From " + senderRe + ", subject " + subjectRe
	case senderRe != "":
		return "From " + senderRe
	default:
		return "Subject " + subjectRe
	}
}

// Rules returns the rules, oldest first; every rule when accountID is "".
func (s *Store) Rules(accountID string) ([]Rule, error) {
	query := `SELECT id, account_id, desktop_id, name, sender_re, subject_re, content, include_body, verified_only,
		enabled, created_ns, last_fired_ns, fired FROM mail_rules`

	args := []any{}
	if accountID != "" {
		query += ` WHERE account_id = ?`
		args = append(args, accountID)
	}

	rows, err := s.db.Query(query+` ORDER BY created_ns`, args...)
	if err != nil {
		return nil, fmt.Errorf("mailtrigger: rules: %w", err)
	}
	defer rows.Close()

	rules := make([]Rule, 0)

	for rows.Next() {
		var r Rule
		var includeBody, verifiedOnly, enabled int
		var created, lastFired int64

		if err := rows.Scan(&r.ID, &r.AccountID, &r.DesktopID, &r.Name, &r.SenderRe, &r.SubjectRe, &r.Content,
			&includeBody, &verifiedOnly, &enabled, &created, &lastFired, &r.Fired); err != nil {
			return nil, fmt.Errorf("mailtrigger: rules: %w", err)
		}

		r.IncludeBody = includeBody != 0
		r.VerifiedOnly = verifiedOnly != 0
		r.Enabled = enabled != 0
		r.CreatedAt = sqlitedb.FromNS(created)
		r.LastFiredAt = sqlitedb.FromNS(lastFired)

		rules = append(rules, r)
	}

	return rules, rows.Err()
}

// SetRuleEnabled pauses or resumes a rule. A paused rule ignores the email
// that arrives while it is paused; it does not catch up on resume.
func (s *Store) SetRuleEnabled(id string, enabled bool) error {
	res, err := s.db.Exec(`UPDATE mail_rules SET enabled = ? WHERE id = ?`, boolInt(enabled), id)
	if err != nil {
		return fmt.Errorf("mailtrigger: enable rule: %w", err)
	}

	return mustAffect(res)
}

// DeleteRule removes a rule and its waiting jobs.
func (s *Store) DeleteRule(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.db.Exec(`DELETE FROM mail_jobs WHERE rule_id = ? AND status = 'pending'`, id); err != nil {
		return fmt.Errorf("mailtrigger: delete rule: %w", err)
	}

	if _, err := s.db.Exec(`DELETE FROM mail_rules WHERE id = ?`, id); err != nil {
		return fmt.Errorf("mailtrigger: delete rule: %w", err)
	}

	return nil
}

// DeleteForDesktop removes every rule that sends to desktopID, and its
// waiting jobs - called when the desktop is deleted, for the reason
// schedule.Store.DeleteForDesktop gives.
func (s *Store) DeleteForDesktop(desktopID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.db.Exec(`DELETE FROM mail_rules WHERE desktop_id = ?`, desktopID); err != nil {
		log.Printf("mailtrigger: delete for desktop %s: %v", desktopID, err)
	}

	if _, err := s.db.Exec(`DELETE FROM mail_jobs WHERE desktop_id = ? AND status = 'pending'`, desktopID); err != nil {
		log.Printf("mailtrigger: delete for desktop %s: %v", desktopID, err)
	}
}
