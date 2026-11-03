package access

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// Policy says which approvals need which approvers. A policy applies to an
// approval when its worker, worker_role, action_match and amount_over all
// match; every policy that applies must be satisfied.
type Policy struct {
	ID string `json:"id"`
	// Worker is a desktop id, or "*" (or "") for every worker.
	Worker string `json:"worker"`
	// WorkerRole limits the policy to workers holding this role; "" is any.
	WorkerRole string `json:"worker_role"`
	// ActionMatch is a case-insensitive regular expression over the
	// approval's action text, e.g. `\b(send|pay|file|delete)\b`; "" matches every approval.
	ActionMatch string `json:"action_match"`
	// AmountOver, when set, applies the policy only when the largest money
	// figure in the action text is over it. Text with no money figure is
	// treated as over (the stricter reading).
	AmountOver *float64 `json:"amount_over,omitempty"`
	// ApproverRoles: an approver must hold one of these (or "owner").
	ApproverRoles []string `json:"approver_roles"`
	// MinApprovers is 1, or 2 for two distinct approvers.
	MinApprovers int       `json:"min_approvers"`
	Note         string    `json:"note"`
	CreatedAt    time.Time `json:"created_at"`
}

// normalize validates p and fills its defaults.
func (p *Policy) normalize() error {
	p.Worker = strings.TrimSpace(p.Worker)
	if p.Worker == "" {
		p.Worker = "*"
	}
	p.WorkerRole = strings.ToLower(strings.TrimSpace(p.WorkerRole))
	if p.WorkerRole != "" && !roleRe.MatchString(p.WorkerRole) {
		return ErrBadRoles
	}
	p.ActionMatch = strings.TrimSpace(p.ActionMatch)
	if len(p.ActionMatch) > 512 {
		return ErrBadMatch
	}
	if _, err := regexp.Compile(`(?i)` + p.ActionMatch); err != nil {
		return ErrBadMatch
	}
	if p.AmountOver != nil && (*p.AmountOver < 0 || math.IsNaN(*p.AmountOver) || math.IsInf(*p.AmountOver, 0)) {
		return ErrBadPolicy
	}
	roles, err := normalizeRoles(p.ApproverRoles)
	if err != nil {
		return err
	}
	if len(roles) == 0 {
		return ErrBadPolicy
	}
	p.ApproverRoles = roles
	if p.MinApprovers == 0 {
		p.MinApprovers = 1
	}
	if p.MinApprovers < 1 || p.MinApprovers > 2 {
		return ErrBadPolicy
	}
	if len(p.Note) > 500 {
		p.Note = p.Note[:500]
	}
	return nil
}

// SavePolicy creates p (empty ID) or replaces the policy with p's ID, and returns it as stored.
func (s *Store) SavePolicy(p Policy) (Policy, error) {
	if err := p.normalize(); err != nil {
		return Policy{}, err
	}
	var amount any
	if p.AmountOver != nil {
		amount = *p.AmountOver
	}

	if p.ID == "" {
		p.ID = sqlitedb.NewID("p_", 8)
		p.CreatedAt = s.now().UTC()
		_, err := s.db.Exec(`INSERT INTO access_policies (id, worker, worker_role, action_match, amount_over, approver_roles, min_approvers, note, created_ns)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			p.ID, p.Worker, p.WorkerRole, p.ActionMatch, amount, encodeList(p.ApproverRoles), p.MinApprovers, p.Note, sqlitedb.ToNS(p.CreatedAt))
		if err != nil {
			return Policy{}, fmt.Errorf("access: create policy: %w", err)
		}
		return p, nil
	}

	res, err := s.db.Exec(`UPDATE access_policies SET worker = ?, worker_role = ?, action_match = ?, amount_over = ?, approver_roles = ?, min_approvers = ?, note = ? WHERE id = ?`,
		p.Worker, p.WorkerRole, p.ActionMatch, amount, encodeList(p.ApproverRoles), p.MinApprovers, p.Note, p.ID)
	if err != nil {
		return Policy{}, fmt.Errorf("access: update policy: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Policy{}, ErrNotFound
	}
	return s.Policy(p.ID)
}

// DeletePolicy removes a policy.
func (s *Store) DeletePolicy(id string) error {
	res, err := s.db.Exec(`DELETE FROM access_policies WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("access: delete policy: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

const policyColumns = `id, worker, worker_role, action_match, amount_over, approver_roles, min_approvers, note, created_ns`

func scanPolicy(row sqlitedb.Scanner) (Policy, error) {
	var p Policy
	var amount sql.NullFloat64
	var roles string
	var created int64
	if err := row.Scan(&p.ID, &p.Worker, &p.WorkerRole, &p.ActionMatch, &amount, &roles, &p.MinApprovers, &p.Note, &created); err != nil {
		return Policy{}, err
	}
	if amount.Valid {
		v := amount.Float64
		p.AmountOver = &v
	}
	p.ApproverRoles = decodeList(roles)
	p.CreatedAt = sqlitedb.FromNS(created)
	return p, nil
}

// Policy returns one policy.
func (s *Store) Policy(id string) (Policy, error) {
	p, err := scanPolicy(s.db.QueryRow(`SELECT `+policyColumns+` FROM access_policies WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Policy{}, ErrNotFound
	}
	return p, err
}

// Policies lists every policy, oldest first.
func (s *Store) Policies() ([]Policy, error) {
	return sqlitedb.QueryAll(s.db, scanPolicy, `SELECT `+policyColumns+` FROM access_policies ORDER BY created_ns, rowid`)
}

// Requirement is what an approval needs: every listed policy satisfied.
type Requirement struct {
	Policies []Policy `json:"policies"`
	// MinApprovers is the largest MinApprovers among Policies (0 when none apply).
	MinApprovers int `json:"min_approvers"`
	// Amount is the largest money figure read from the action text, if any.
	Amount *float64 `json:"amount,omitempty"`
	// AmountUnknown says a policy with an amount limit applies only because no amount could be read for certain
	// (Amount is nil, and an unknown amount fails closed): the approver is told so (REVIEW-3 follow-up 6).
	AmountUnknown bool `json:"amount_unknown,omitempty"`
}

// amountNote is the sentence an approver is shown when an amount policy applied because the amount is unknown.
func amountNote(req Requirement) string {
	if !req.AmountUnknown {
		return ""
	}
	return " The amount could not be read for certain, so the policy for larger amounts applied."
}

// None reports whether no policy applies.
func (r Requirement) None() bool { return len(r.Policies) == 0 }

// Applies reports whether p applies to an approval by worker (holding workerRoles) for actionText with amount.
func (p Policy) Applies(worker string, workerRoles []string, actionText string, amount *float64) bool {
	if p.Worker != "" && p.Worker != "*" && !strings.EqualFold(p.Worker, worker) {
		return false
	}
	if p.WorkerRole != "" {
		found := false
		for _, r := range workerRoles {
			if strings.EqualFold(r, p.WorkerRole) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if p.ActionMatch != "" {
		re, err := regexp.Compile(`(?i)` + p.ActionMatch)
		if err != nil {
			return true // a stored pattern that no longer compiles fails closed: it applies
		}
		if !re.MatchString(actionText) {
			return false
		}
	}
	if p.AmountOver != nil && amount != nil && *amount <= *p.AmountOver {
		return false
	}
	return true
}

// Required returns which policies apply to an approval by worker for actionText, and how many approvers it needs.
func (s *Store) Required(worker string, workerRoles []string, actionText string) (Requirement, error) {
	policies, err := s.Policies()
	if err != nil {
		return Requirement{}, err
	}
	req := Requirement{Policies: []Policy{}, Amount: LargestAmount(actionText)}
	for _, p := range policies {
		if p.Applies(worker, workerRoles, actionText, req.Amount) {
			req.Policies = append(req.Policies, p)
			if p.AmountOver != nil && req.Amount == nil {
				req.AmountUnknown = true
			}
			if p.MinApprovers > req.MinApprovers {
				req.MinApprovers = p.MinApprovers
			}
		}
	}
	return req, nil
}

// CanApprove reports whether u may approve under every policy in req. The
// local owner and any user holding "owner" may; a disabled user may not.
func CanApprove(u User, req Requirement) bool {
	if u.Local {
		return true
	}
	if u.ID == "" || u.Disabled {
		return req.None()
	}
	if u.HasRole(RoleOwner) {
		return true
	}
	for _, p := range req.Policies {
		ok := false
		for _, role := range p.ApproverRoles {
			if u.HasRole(role) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}
