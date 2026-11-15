package knowledge

// Proposals: what a worker's review pass proposed after a run it was checked done on (docs/learning-loop.md, "The
// review pass"; docs/skillhub.md). A proposal is staged here for a person and is never given to a worker while it is
// proposed: it lives in a table of its own, which ForWorker and ForTask never read. A person approves it - it becomes
// an item with that person as its source (plan rule 5) - or rejects it, and either way the row stays. A proposal that
// would replace an item is approved only while that item is still live with the very text the worker quoted (read
// before write): one replaced or retired since is refused as changed.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// Proposal states.
const (
	StateProposed = "proposed"
	StateApproved = "approved"
	StateRejected = "rejected"
)

// Proposal kinds: about the person or the work, and how an application is operated.
const (
	ProposalLesson  = "lesson"
	ProposalAppNote = "app_note"
)

// proposalKinds maps a proposal's kind to the kind of item it becomes when approved.
var proposalKinds = map[string]string{ProposalLesson: KindLesson, ProposalAppNote: KindFact}

// Evidence places: the run's own transcript, or the person's own words in the task record.
const (
	EvidenceRun    = "run"
	EvidencePerson = "person"
)

// Bounds on a proposal.
const (
	MaxProposalsPerRun = 3
	MaxEvidence        = 3
	MaxQuote           = 500
	MaxProposalText    = 1000
	MaxWhy             = 1000
	MaxNote            = 1000
)

// ErrChanged is an approval refused because the item the proposal replaces changed since it was proposed.
var ErrChanged = errors.New("it changed since this was proposed")

// ErrDuplicate is a proposal of something already known, already proposed, or already rejected.
var ErrDuplicate = errors.New("duplicate")

// Evidence is one exact quote a proposal rests on, and where it stands.
type Evidence struct {
	Quote string `json:"quote"`
	Where string `json:"where"`
}

// Proposal is one thing a worker proposed, with its provenance and what a person decided.
type Proposal struct {
	ID       string     `json:"id"`
	Worker   string     `json:"worker"`
	TaskID   string     `json:"task_id"`
	Kind     string     `json:"kind"`
	Text     string     `json:"text"`
	Why      string     `json:"why"`
	Evidence []Evidence `json:"evidence"`
	// ReplacesID is the live item the proposal would replace, found by its exact text (ReplacesText) when proposed.
	ReplacesID   string `json:"replaces_id,omitempty"`
	ReplacesText string `json:"replaces_text,omitempty"`
	// CallID and Model name the model call that proposed it, when known.
	CallID    string    `json:"call_id,omitempty"`
	Model     string    `json:"model,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	State     string    `json:"state"`
	// DecidedAt, DecidedBy (an actor id) and DecidedByName say who approved or rejected it, and when.
	DecidedAt     *time.Time `json:"decided_at,omitempty"`
	DecidedBy     string     `json:"decided_by,omitempty"`
	DecidedByName string     `json:"decided_by_name,omitempty"`
	Note          string     `json:"note,omitempty"`
	// ItemID is the item an approval made.
	ItemID string `json:"item_id,omitempty"`
}

// Decider is the person who approves or rejects: an actor id, and a name to show.
type Decider struct {
	ID   string
	Name string
}

// collapse is s with every run of white space one space, trimmed.
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// sameWords reports whether a and b say the same, case and spacing aside.
func sameWords(a, b string) bool { return strings.EqualFold(collapse(a), collapse(b)) }

// printable reports whether s is UTF-8 of at most max bytes with no control characters.
func printable(s string, max int) bool {
	if len(s) > max || !utf8.ValidString(s) {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// cleanProposal checks a proposal before it is stored, its text fields collapsed.
func cleanProposal(p Proposal) (Proposal, error) {
	p.Worker, p.TaskID, p.Kind = strings.TrimSpace(p.Worker), strings.TrimSpace(p.TaskID), strings.TrimSpace(p.Kind)
	p.Text, p.Why, p.ReplacesText = collapse(p.Text), collapse(p.Why), collapse(p.ReplacesText)
	p.CallID, p.Model = strings.TrimSpace(p.CallID), strings.TrimSpace(p.Model)
	if !keyPattern.MatchString(p.Worker) || !keyPattern.MatchString(p.TaskID) {
		return p, invalid("a proposal names the worker and the task it came from")
	}
	if _, ok := proposalKinds[p.Kind]; !ok {
		return p, invalid("a proposal's kind is lesson or app_note")
	}
	if p.Text == "" || !printable(p.Text, MaxProposalText) {
		return p, invalid("a proposal's text is one printable line of at most %d bytes", MaxProposalText)
	}
	if !printable(p.Why, MaxWhy) || !printable(p.ReplacesText, MaxText) {
		return p, invalid("a proposal's why or replaced text is too long or not printable")
	}
	if len(p.CallID) > 128 || len(p.Model) > 128 || !printable(p.CallID, 128) || !printable(p.Model, 128) {
		return p, invalid("the call that proposed it is not named in a short printable line")
	}
	if len(p.Evidence) == 0 || len(p.Evidence) > MaxEvidence {
		return p, invalid("a proposal rests on 1 to %d quotes", MaxEvidence)
	}
	for i, e := range p.Evidence {
		e.Quote = strings.TrimSpace(e.Quote)
		if e.Quote == "" || !utf8.ValidString(e.Quote) || len(e.Quote) > MaxQuote {
			return p, invalid("each quote is at most %d bytes of UTF-8", MaxQuote)
		}
		if e.Where != EvidenceRun && e.Where != EvidencePerson {
			return p, invalid("each quote stands in the run or in the person's words")
		}
		p.Evidence[i] = e
	}
	return p, nil
}

const proposalColumns = `id, worker, task_id, kind, text, why, evidence, replaces_id, replaces_text, call_id, model,
	created_ns, state, decided_ns, decided_by, decided_name, note, item_id`

// scanProposal reads one proposal row.
func scanProposal(row sqlitedb.Scanner) (Proposal, error) {
	var p Proposal
	var evidence string
	var created, decided int64
	if err := row.Scan(&p.ID, &p.Worker, &p.TaskID, &p.Kind, &p.Text, &p.Why, &evidence, &p.ReplacesID, &p.ReplacesText,
		&p.CallID, &p.Model, &created, &p.State, &decided, &p.DecidedBy, &p.DecidedByName, &p.Note, &p.ItemID); err != nil {
		return Proposal{}, err
	}
	p.CreatedAt = sqlitedb.FromNS(created)
	if decided != 0 {
		at := sqlitedb.FromNS(decided)
		p.DecidedAt = &at
	}
	if err := json.Unmarshal([]byte(evidence), &p.Evidence); err != nil || p.Evidence == nil {
		p.Evidence = []Evidence{}
	}
	return p, nil
}

// Propose stages a worker's proposal for a person. roles are the worker's roles: a proposal is refused as a duplicate
// of an item the worker is already given (org, its roles, its own), of a proposal of its own still waiting, or of one
// a person rejected; a ReplacesText must be the exact text (spacing aside) of one of those live items; and a run
// proposes at most MaxProposalsPerRun.
func (s *Store) Propose(p Proposal, roles []string) (Proposal, error) {
	p, err := cleanProposal(p)
	if err != nil {
		return Proposal{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	given, err := s.ForWorker(p.Worker, roles)
	if err != nil {
		return Proposal{}, err
	}
	p.ReplacesID = ""
	for _, it := range given {
		if sameWords(it.Text, p.Text) {
			return Proposal{}, fmt.Errorf("%w: the worker is already given that", ErrDuplicate)
		}
		if p.ReplacesText != "" && p.ReplacesID == "" && collapse(it.Text) == p.ReplacesText {
			p.ReplacesID = it.ID
		}
	}
	if p.ReplacesText != "" && p.ReplacesID == "" {
		return Proposal{}, invalid("it replaces no line this worker is given: quote one exactly")
	}

	earlier, err := sqlitedb.QueryAll(s.db, scanProposal, `SELECT `+proposalColumns+` FROM knowledge_proposals
		WHERE worker = ? AND (state != ? OR task_id = ?)`, p.Worker, StateApproved, p.TaskID)
	if err != nil {
		return Proposal{}, fmt.Errorf("knowledge: proposals: %w", err)
	}
	run := 0
	for _, e := range earlier {
		if e.TaskID == p.TaskID {
			run++
		}
		if e.State != StateApproved && sameWords(e.Text, p.Text) {
			return Proposal{}, fmt.Errorf("%w: that was already %s", ErrDuplicate, e.State)
		}
	}
	if run >= MaxProposalsPerRun {
		return Proposal{}, invalid("a run proposes at most %d things", MaxProposalsPerRun)
	}

	evidence, err := json.Marshal(p.Evidence)
	if err != nil {
		return Proposal{}, fmt.Errorf("knowledge: proposal evidence: %w", err)
	}
	p.ID = sqlitedb.NewID("kp_", 8)
	p.CreatedAt = time.Now().UTC()
	p.State = StateProposed
	p.DecidedAt, p.DecidedBy, p.DecidedByName, p.Note, p.ItemID = nil, "", "", "", ""
	if _, err := s.db.Exec(`INSERT INTO knowledge_proposals (id, worker, task_id, kind, text, why, evidence, replaces_id,
		replaces_text, call_id, model, created_ns, state) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Worker, p.TaskID, p.Kind, p.Text, p.Why, string(evidence), p.ReplacesID, p.ReplacesText, p.CallID,
		p.Model, sqlitedb.ToNS(p.CreatedAt), p.State); err != nil {
		return Proposal{}, fmt.Errorf("knowledge: propose: %w", err)
	}
	return p, nil
}

// GetProposal returns one proposal, whatever its state.
func (s *Store) GetProposal(id string) (Proposal, error) {
	p, err := scanProposal(s.db.QueryRow(`SELECT `+proposalColumns+` FROM knowledge_proposals WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Proposal{}, ErrNotFound
	}
	if err != nil {
		return Proposal{}, fmt.Errorf("knowledge: proposal: %w", err)
	}
	return p, nil
}

// Proposals lists proposals in state ("" for every state), newest first, at most limit (1 to 500; 200 when out of
// range).
func (s *Store) Proposals(state string, limit int) ([]Proposal, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	query, args := `SELECT `+proposalColumns+` FROM knowledge_proposals`, []any{}
	if state != "" {
		query += ` WHERE state = ?`
		args = append(args, state)
	}
	query += ` ORDER BY created_ns DESC, id DESC LIMIT ?`
	args = append(args, limit)
	out, err := sqlitedb.QueryAll(s.db, scanProposal, query, args...)
	if err != nil {
		return nil, fmt.Errorf("knowledge: proposals: %w", err)
	}
	if out == nil {
		out = []Proposal{}
	}
	return out, nil
}

// Approve makes a waiting proposal an item, with by as its source (a person, plan rule 5), and marks the proposal
// approved. A proposal that replaces an item supersedes it, in that item's scope, only while the item is still live
// with the text that was quoted (ErrChanged otherwise, and the proposal stays waiting). Any other is scoped as scope
// and scopeKey say: the proposing worker by default, one of the org or a role when the person chooses; never another
// worker's or a task's.
func (s *Store) Approve(id string, by Decider, scope, scopeKey string) (Item, Proposal, error) {
	by.ID, by.Name = strings.TrimSpace(by.ID), collapse(by.Name)
	if by.ID == "" || !refPattern.MatchString(by.ID) || !refPattern.MatchString(by.Name) {
		return Item{}, Proposal{}, invalid("an approval names the person who approves")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	p, err := s.GetProposal(id)
	if err != nil {
		return Item{}, Proposal{}, err
	}
	if p.State != StateProposed {
		return Item{}, Proposal{}, invalid("that proposal was already %s", p.State)
	}
	next := Item{Kind: proposalKinds[p.Kind], Text: p.Text,
		Source: Source{Kind: SourcePerson, Ref: by.ID, Worker: p.Worker, TaskID: p.TaskID}}
	if p.ReplacesID != "" {
		old, err := s.Get(p.ReplacesID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return Item{}, Proposal{}, err
		}
		if err != nil || old.SupersededBy != "" || collapse(old.Text) != p.ReplacesText {
			return Item{}, Proposal{}, ErrChanged
		}
		next.Scope, next.ScopeKey = old.Scope, old.ScopeKey
	} else {
		switch scope = strings.TrimSpace(scope); scope {
		case "", ScopeWorker:
			next.Scope, next.ScopeKey = ScopeWorker, p.Worker
			if k := strings.TrimSpace(scopeKey); k != "" && k != p.Worker {
				return Item{}, Proposal{}, invalid("a proposal is approved for the worker that proposed it, a role, or everyone")
			}
		case ScopeOrg:
			next.Scope, next.ScopeKey = ScopeOrg, ""
		case ScopeRole:
			next.Scope, next.ScopeKey = ScopeRole, strings.TrimSpace(scopeKey)
		default:
			return Item{}, Proposal{}, invalid("a proposal is approved for the worker that proposed it, a role, or everyone")
		}
	}
	next, err = clean(next)
	if err != nil {
		return Item{}, Proposal{}, err
	}
	next.ID = sqlitedb.NewID("kn_", 8)
	next.CreatedAt = time.Now().UTC()

	tx, err := s.db.Begin()
	if err != nil {
		return Item{}, Proposal{}, fmt.Errorf("knowledge: approve: %w", err)
	}
	defer tx.Rollback()
	if err := insert(tx, next); err != nil {
		return Item{}, Proposal{}, err
	}
	if p.ReplacesID != "" {
		// Only the very item quoted, still live (a Retire does not take mu): otherwise it changed.
		res, err := tx.Exec(`UPDATE knowledge_items SET superseded_by = ? WHERE id = ? AND superseded_by = ''`, next.ID, p.ReplacesID)
		if err != nil {
			return Item{}, Proposal{}, fmt.Errorf("knowledge: approve: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return Item{}, Proposal{}, ErrChanged
		}
	}
	now := time.Now().UTC()
	res, err := tx.Exec(`UPDATE knowledge_proposals SET state = ?, decided_ns = ?, decided_by = ?, decided_name = ?, item_id = ?
		WHERE id = ? AND state = ?`, StateApproved, sqlitedb.ToNS(now), by.ID, by.Name, next.ID, p.ID, StateProposed)
	if err != nil {
		return Item{}, Proposal{}, fmt.Errorf("knowledge: approve: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Item{}, Proposal{}, invalid("that proposal was decided meanwhile")
	}
	if err := tx.Commit(); err != nil {
		return Item{}, Proposal{}, fmt.Errorf("knowledge: approve: %w", err)
	}
	p.State, p.DecidedAt, p.DecidedBy, p.DecidedByName, p.ItemID = StateApproved, &now, by.ID, by.Name, next.ID
	return next, p, nil
}

// Reject marks a waiting proposal rejected by by, with an optional note. The row stays: what was proposed and why it
// was turned down is kept, and the same text proposed again is refused as a duplicate.
func (s *Store) Reject(id string, by Decider, note string) (Proposal, error) {
	by.ID, by.Name, note = strings.TrimSpace(by.ID), collapse(by.Name), collapse(note)
	if by.ID == "" || !refPattern.MatchString(by.ID) || !refPattern.MatchString(by.Name) {
		return Proposal{}, invalid("a rejection names the person who rejects")
	}
	if !printable(note, MaxNote) {
		return Proposal{}, invalid("the note is at most %d bytes", MaxNote)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	res, err := s.db.Exec(`UPDATE knowledge_proposals SET state = ?, decided_ns = ?, decided_by = ?, decided_name = ?, note = ?
		WHERE id = ? AND state = ?`, StateRejected, sqlitedb.ToNS(now), by.ID, by.Name, note, id, StateProposed)
	if err != nil {
		return Proposal{}, fmt.Errorf("knowledge: reject: %w", err)
	}
	p, err := s.GetProposal(id)
	if err != nil {
		return Proposal{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Proposal{}, invalid("that proposal was already %s", p.State)
	}
	return p, nil
}
