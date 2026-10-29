package access

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// Reply kinds: what a person's reply to a pending approval says.
const (
	ReplyYes   = "yes"
	ReplyNo    = "no"
	ReplyOther = "other"
)

// Outcomes of a decision.
const (
	OutcomeAllowed = "allowed" // the reply goes to the worker
	OutcomeHeld    = "held"    // a first yes, kept until a second approver agrees
	OutcomeRefused = "refused" // the reply is not delivered
)

// anonymousID is what an unidentified actor's decision is logged under.
const anonymousID = "anonymous"

var (
	yesRe = regexp.MustCompile(`^(yes|y|yeah|yep|ok|okay|approve|approved|go ahead|do it|confirm|confirmed|proceed|sure)\b`)
	// noRe is a plain refusal and nothing else: a "no, do X instead" is an instruction (ReplyOther).
	noRe = regexp.MustCompile(`^(no|n|nope|don'?t|do not|stop|cancel|reject|rejected|deny|denied|decline|declined|abort)( (do it|it|that|this|please|thanks|thank you))*$`)
	// punctRe trims what ends or wraps a reply.
	punctRe = regexp.MustCompile(`[\s.!,;:"'()]+$|^[\s"'(]+`)
	spaceRe = regexp.MustCompile(`\s+`)
)

// ClassifyReply reads a reply to an approval as a yes, a plain no, or something else (an instruction).
func ClassifyReply(reply string) string {
	text := strings.ToLower(strings.TrimSpace(reply))
	text = spaceRe.ReplaceAllString(punctRe.ReplaceAllString(text, ""), " ")
	text = strings.ReplaceAll(text, "’", "'")
	switch {
	case noRe.MatchString(text):
		return ReplyNo
	case yesRe.MatchString(text) && !strings.Contains(text, " not ") && !strings.Contains(text, "don't") && !strings.Contains(text, "dont"):
		return ReplyYes
	default:
		return ReplyOther
	}
}

// Decision is what Decide made of a reply.
type Decision struct {
	// Allowed: deliver the reply to the worker.
	Allowed bool `json:"allowed"`
	// RecordedAs is the reply's kind as logged: yes, no or other.
	RecordedAs string `json:"recorded_as"`
	// Outcome is allowed, held or refused.
	Outcome string `json:"outcome"`
	// Message is for the person when the reply was not delivered.
	Message     string      `json:"message,omitempty"`
	Requirement Requirement `json:"requirement"`
}

// Approval is one entry of the approvals log.
type Approval struct {
	ID            string    `json:"id"`
	Worker        string    `json:"worker"`
	TaskID        string    `json:"task_id"`
	Action        string    `json:"action"`
	DecidedBy     string    `json:"decided_by"`
	DecidedByName string    `json:"decided_by_name"`
	Decision      string    `json:"decision"`
	Outcome       string    `json:"outcome"`
	Policies      []string  `json:"policies"`
	At            time.Time `json:"at"`
}

// pendingKey names one pending approval: the same action text on the same task of the same worker.
func pendingKey(worker, taskID, action string) string {
	norm := spaceRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(action)), " ")
	sum := sha256.Sum256([]byte(worker + "\x00" + taskID + "\x00" + norm))
	return hex.EncodeToString(sum[:])
}

// Decide settles a person's reply to a worker's pending approval: it finds
// the policies that apply, refuses a reply from someone who may not approve,
// holds a first yes where two approvers are needed, and logs every decision
// with who made it. workerRoles nil means the roles stored for worker. A
// plain "no" is always allowed: stopping is never the risky direction.
func (s *Store) Decide(actor User, worker string, workerRoles []string, taskID, pendingActionText, replyText string) (Decision, error) {
	if workerRoles == nil {
		workerRoles = s.WorkerRoles(worker)
	}
	req, err := s.Required(worker, workerRoles, pendingActionText)
	if err != nil {
		return Decision{}, err
	}

	d := Decision{RecordedAs: ClassifyReply(replyText), Requirement: req}
	key := pendingKey(worker, taskID, pendingActionText)

	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case req.None() || d.RecordedAs == ReplyNo:
		d.Allowed, d.Outcome = true, OutcomeAllowed

	case !CanApprove(actor, req):
		d.Outcome = OutcomeRefused
		d.Message = "This needs approval from " + describe(req) + "."
		if actor.ID == "" {
			d.Message += " Sign in with your own name to approve it."
		}

	case actor.Local || req.MinApprovers <= 1:
		d.Allowed, d.Outcome = true, OutcomeAllowed

	case d.RecordedAs == ReplyOther:
		d.Outcome = OutcomeRefused
		d.Message = "This needs two approvers, so reply yes or no. To change what it does, reply no and then send the new instruction."

	default:
		held, err := s.heldYeses(key)
		if err != nil {
			return Decision{}, err
		}
		switch {
		case held[actor.ID]:
			d.Outcome = OutcomeRefused
			d.Message = "You have already approved this. It needs a second, different approver: " + describe(req) + "."
		case len(held) > 0:
			d.Allowed, d.Outcome = true, OutcomeAllowed
		default:
			d.Outcome = OutcomeHeld
			d.Message = "Your yes is recorded. This needs a second approver (" + describe(req) + ") before it goes ahead."
		}
	}

	if err := s.record(actor, worker, taskID, pendingActionText, key, d); err != nil {
		return Decision{}, err
	}
	return d, nil
}

// heldYeses returns the users whose yes is held on key since the pending approval was last settled.
func (s *Store) heldYeses(key string) (map[string]bool, error) {
	type row struct{ by, decision, outcome string }
	rows, err := sqlitedb.QueryAll(s.db, func(r sqlitedb.Scanner) (row, error) {
		var x row
		err := r.Scan(&x.by, &x.decision, &x.outcome)
		return x, err
	}, `SELECT decided_by, decision, outcome FROM access_approvals WHERE pending_key = ? ORDER BY at_ns DESC, rowid DESC LIMIT 200`, key)
	if err != nil {
		return nil, fmt.Errorf("access: held approvals: %w", err)
	}
	held := map[string]bool{}
	for _, x := range rows {
		if x.outcome == OutcomeAllowed {
			break // settled: anything older belongs to an earlier round
		}
		if x.outcome == OutcomeHeld && x.decision == ReplyYes {
			held[x.by] = true
		}
	}
	return held, nil
}

// record appends a decision to the approvals log.
func (s *Store) record(actor User, worker, taskID, action, key string, d Decision) error {
	by, name := actor.ID, actor.Name
	if by == "" {
		by, name = anonymousID, "Not signed in"
	}
	ids := make([]string, 0, len(d.Requirement.Policies))
	for _, p := range d.Requirement.Policies {
		ids = append(ids, p.ID)
	}
	if len(action) > 4000 {
		action = action[:4000]
	}
	_, err := s.db.Exec(`INSERT INTO access_approvals (id, worker, task_id, action, pending_key, decided_by, decided_by_name, decision, outcome, policies, at_ns)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sqlitedb.NewID("a_", 10), worker, taskID, action, key, by, name, d.RecordedAs, d.Outcome, encodeList(ids), sqlitedb.ToNS(s.now()))
	if err != nil {
		return fmt.Errorf("access: record decision: %w", err)
	}
	return nil
}

// RecordDecision logs a decision made outside Decide (for example a reply the control plane delivered without a policy check).
func (s *Store) RecordDecision(actor User, worker, taskID, action, decision, outcome string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.record(actor, worker, taskID, action, pendingKey(worker, taskID, action),
		Decision{RecordedAs: decision, Outcome: outcome, Requirement: Requirement{}})
}

// Approvals returns the newest entries of the approvals log, optionally for one worker.
func (s *Store) Approvals(worker string, limit int) ([]Approval, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	scan := func(r sqlitedb.Scanner) (Approval, error) {
		var a Approval
		var policies string
		var at int64
		err := r.Scan(&a.ID, &a.Worker, &a.TaskID, &a.Action, &a.DecidedBy, &a.DecidedByName, &a.Decision, &a.Outcome, &policies, &at)
		a.Policies = decodeList(policies)
		a.At = sqlitedb.FromNS(at)
		return a, err
	}
	const cols = `id, worker, task_id, action, decided_by, decided_by_name, decision, outcome, policies, at_ns`
	if worker != "" {
		return sqlitedb.QueryAll(s.db, scan, `SELECT `+cols+` FROM access_approvals WHERE worker = ? ORDER BY at_ns DESC, rowid DESC LIMIT ?`, worker, limit)
	}
	return sqlitedb.QueryAll(s.db, scan, `SELECT `+cols+` FROM access_approvals ORDER BY at_ns DESC, rowid DESC LIMIT ?`, limit)
}

// describe names who may approve under req, e.g. "a finance or treasury approver".
func describe(req Requirement) string {
	parts := make([]string, 0, len(req.Policies))
	seen := map[string]bool{}
	for _, p := range req.Policies {
		phrase := strings.Join(p.ApproverRoles, " or ")
		if seen[phrase] {
			continue
		}
		seen[phrase] = true
		parts = append(parts, phrase)
	}
	if len(parts) == 0 {
		return "an approver"
	}
	article := "a"
	if strings.ContainsAny(parts[0][:1], "aeiou") {
		article = "an"
	}
	return article + " " + strings.Join(parts, " and ") + " approver"
}
