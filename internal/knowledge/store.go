// Package knowledge holds what workers are told to know - rules, facts,
// corrections and lessons - each scoped (to one task, one worker, a role, or
// the whole organisation) and each carrying where it came from: a person, a
// demonstration, a run, or an import. An item is never edited: a change is a
// new item that supersedes the old one, so Why can always show how a line a
// worker was given came to be.
package knowledge

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// Scopes, broadest first.
const (
	ScopeOrg    = "org"
	ScopeRole   = "role"
	ScopeWorker = "worker"
	ScopeTask   = "task"
)

// Kinds of item.
const (
	KindRule       = "rule"
	KindFact       = "fact"
	KindCorrection = "correction"
	KindLesson     = "lesson"
)

// Kinds of source.
const (
	SourcePerson        = "person"
	SourceDemonstration = "demonstration"
	SourceRun           = "run"
	SourceImport        = "import"
)

// MaxText bounds one item's text: a rule is a sentence or a paragraph, never a document.
const MaxText = 4000

// maxChain bounds how far Why walks back through superseded items.
const maxChain = 100

// retired is the superseded_by mark of an item withdrawn with no replacement.
const retired = "-"

var (
	scopes  = map[string]bool{ScopeOrg: true, ScopeRole: true, ScopeWorker: true, ScopeTask: true}
	kinds   = map[string]bool{KindRule: true, KindFact: true, KindCorrection: true, KindLesson: true}
	sources = map[string]bool{SourcePerson: true, SourceDemonstration: true, SourceRun: true, SourceImport: true}

	// keyPattern keeps a scope key (a role, worker or task id) a short plain label.
	keyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _.:-]{0,127}$`)
	// refPattern keeps a source reference printable and short.
	refPattern = regexp.MustCompile(`^[^\x00-\x1f]{0,256}$`)
)

// ErrInvalid marks input the store refused; its text is safe to show.
var ErrInvalid = errors.New("invalid")

// ErrNotFound marks an item the store does not have.
var ErrNotFound = errors.New("not found")

// invalid builds an ErrInvalid with words a person can act on.
func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

var schema = []string{
	`CREATE TABLE knowledge_items (
		id            TEXT PRIMARY KEY,
		scope         TEXT NOT NULL,
		scope_key     TEXT NOT NULL,
		kind          TEXT NOT NULL,
		text          TEXT NOT NULL,
		source_kind   TEXT NOT NULL,
		source_ref    TEXT NOT NULL,
		source_worker TEXT NOT NULL,
		source_task   TEXT NOT NULL,
		created_ns    INTEGER NOT NULL,
		superseded_by TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX knowledge_items_scope ON knowledge_items (scope, scope_key, superseded_by, created_ns)`,
	`CREATE INDEX knowledge_items_superseded ON knowledge_items (superseded_by)`,
	// What workers proposed after a run (proposals.go): staged for a person, never given out while proposed.
	`CREATE TABLE knowledge_proposals (
		id            TEXT PRIMARY KEY,
		worker        TEXT NOT NULL,
		task_id       TEXT NOT NULL,
		kind          TEXT NOT NULL,
		text          TEXT NOT NULL,
		why           TEXT NOT NULL,
		evidence      TEXT NOT NULL,
		replaces_id   TEXT NOT NULL,
		replaces_text TEXT NOT NULL,
		call_id       TEXT NOT NULL,
		model         TEXT NOT NULL,
		created_ns    INTEGER NOT NULL,
		state         TEXT NOT NULL,
		decided_ns    INTEGER NOT NULL DEFAULT 0,
		decided_by    TEXT NOT NULL DEFAULT '',
		decided_name  TEXT NOT NULL DEFAULT '',
		note          TEXT NOT NULL DEFAULT '',
		item_id       TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX knowledge_proposals_state ON knowledge_proposals (state, created_ns)`,
	`CREATE INDEX knowledge_proposals_run ON knowledge_proposals (worker, task_id)`,
}

// Source is where an item came from. Ref names it in its own terms (a
// recording id, a run's task id, an import's file name, a person's name).
type Source struct {
	Kind   string `json:"kind"`
	Ref    string `json:"ref,omitempty"`
	Worker string `json:"worker,omitempty"`
	TaskID string `json:"task_id,omitempty"`
}

// Item is one piece of knowledge.
type Item struct {
	ID           string    `json:"id"`
	Scope        string    `json:"scope"`
	ScopeKey     string    `json:"scope_key"`
	Kind         string    `json:"kind"`
	Text         string    `json:"text"`
	Source       Source    `json:"source"`
	CreatedAt    time.Time `json:"created_at"`
	SupersededBy string    `json:"superseded_by,omitempty"`
}

// Line is the item as a worker is told it: "Rule (org): ...".
func (it Item) Line() string {
	kind := it.Kind
	if kind != "" {
		kind = strings.ToUpper(kind[:1]) + kind[1:]
	}

	where := it.Scope
	if it.Scope == ScopeRole {
		where = "role " + it.ScopeKey
	}

	return fmt.Sprintf("%s (%s): %s", kind, where, strings.Join(strings.Fields(it.Text), " "))
}

// Store keeps knowledge items in SQLite.
type Store struct {
	db *sql.DB
	mu sync.Mutex // Supersede reads then writes
}

// NewStore opens the knowledge store on db, creating its table if needed.
func NewStore(db *sql.DB) (*Store, error) {
	if err := sqlitedb.Migrate(db, "knowledge", schema); err != nil {
		return nil, err
	}

	return &Store{db: db}, nil
}

// clean checks an item before it is stored and trims its text fields.
func clean(it Item) (Item, error) {
	it.Scope = strings.TrimSpace(it.Scope)
	it.ScopeKey = strings.TrimSpace(it.ScopeKey)
	it.Kind = strings.TrimSpace(it.Kind)
	it.Text = strings.TrimSpace(it.Text)
	it.Source.Kind = strings.TrimSpace(it.Source.Kind)
	it.Source.Ref = strings.TrimSpace(it.Source.Ref)
	it.Source.Worker = strings.TrimSpace(it.Source.Worker)
	it.Source.TaskID = strings.TrimSpace(it.Source.TaskID)

	if !scopes[it.Scope] {
		return it, invalid("scope must be task, worker, role or org")
	}
	if it.Scope == ScopeOrg {
		if it.ScopeKey != "" {
			return it, invalid("an org item takes no scope key")
		}
	} else if !keyPattern.MatchString(it.ScopeKey) {
		return it, invalid("a %s item needs the %s it is for as its scope key", it.Scope, it.Scope)
	}
	if !kinds[it.Kind] {
		return it, invalid("kind must be rule, fact, correction or lesson")
	}
	if it.Text == "" {
		return it, invalid("the text is empty")
	}
	if !utf8.ValidString(it.Text) || len(it.Text) > MaxText {
		return it, invalid("the text must be UTF-8 and at most %d bytes", MaxText)
	}
	if !sources[it.Source.Kind] {
		return it, invalid("source kind must be person, demonstration, run or import")
	}
	for _, field := range []string{it.Source.Ref, it.Source.Worker, it.Source.TaskID} {
		if !utf8.ValidString(field) || !refPattern.MatchString(field) {
			return it, invalid("a source field is too long or not printable")
		}
	}

	return it, nil
}

// Add stores a new item and returns it with its id and time.
func (s *Store) Add(it Item) (Item, error) {
	it, err := clean(it)
	if err != nil {
		return Item{}, err
	}

	it.ID = sqlitedb.NewID("kn_", 8)
	it.CreatedAt = time.Now().UTC()
	it.SupersededBy = ""

	if err := insert(s.db, it); err != nil {
		return Item{}, err
	}

	return it, nil
}

// execer is a *sql.DB or *sql.Tx.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// insert writes one item row.
func insert(db execer, it Item) error {
	if _, err := db.Exec(
		`INSERT INTO knowledge_items (id, scope, scope_key, kind, text, source_kind, source_ref, source_worker, source_task, created_ns)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		it.ID, it.Scope, it.ScopeKey, it.Kind, it.Text, it.Source.Kind, it.Source.Ref, it.Source.Worker, it.Source.TaskID,
		sqlitedb.ToNS(it.CreatedAt),
	); err != nil {
		return fmt.Errorf("knowledge: add: %w", err)
	}

	return nil
}

const columns = `id, scope, scope_key, kind, text, source_kind, source_ref, source_worker, source_task, created_ns, superseded_by`

// scanItem reads one item row.
func scanItem(row sqlitedb.Scanner) (Item, error) {
	var it Item
	var ns int64
	if err := row.Scan(&it.ID, &it.Scope, &it.ScopeKey, &it.Kind, &it.Text,
		&it.Source.Kind, &it.Source.Ref, &it.Source.Worker, &it.Source.TaskID, &ns, &it.SupersededBy); err != nil {
		return Item{}, err
	}
	it.CreatedAt = sqlitedb.FromNS(ns)

	return it, nil
}

// Get returns one item, superseded or not.
func (s *Store) Get(id string) (Item, error) {
	it, err := scanItem(s.db.QueryRow(`SELECT `+columns+` FROM knowledge_items WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, ErrNotFound
	}
	if err != nil {
		return Item{}, fmt.Errorf("knowledge: get: %w", err)
	}

	return it, nil
}

// Supersede replaces the live item oldID with next, which keeps oldID's scope
// (and its kind, unless next names one), and returns next as stored.
func (s *Store) Supersede(oldID string, next Item) (Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	old, err := s.Get(oldID)
	if err != nil {
		return Item{}, err
	}
	if old.SupersededBy == retired {
		return Item{}, invalid("that item was retired; add a new one instead")
	}
	if old.SupersededBy != "" {
		return Item{}, invalid("that item was already replaced by %s; replace that one instead", old.SupersededBy)
	}

	next.Scope, next.ScopeKey = old.Scope, old.ScopeKey
	if strings.TrimSpace(next.Kind) == "" {
		next.Kind = old.Kind
	}
	next, err = clean(next)
	if err != nil {
		return Item{}, err
	}
	next.ID = sqlitedb.NewID("kn_", 8)
	next.CreatedAt = time.Now().UTC()
	next.SupersededBy = ""

	tx, err := s.db.Begin()
	if err != nil {
		return Item{}, fmt.Errorf("knowledge: supersede: %w", err)
	}
	defer tx.Rollback()

	if err := insert(tx, next); err != nil {
		return Item{}, err
	}
	// Only a still-live item is replaced: a Retire (which does not take mu) landing between the read above and this
	// write must not be undone, putting a retired line back into force (C-24).
	res, err := tx.Exec(`UPDATE knowledge_items SET superseded_by = ? WHERE id = ? AND superseded_by = ''`, next.ID, oldID)
	if err != nil {
		return Item{}, fmt.Errorf("knowledge: supersede: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Item{}, invalid("that item was replaced or retired meanwhile; read it again")
	}
	if err := tx.Commit(); err != nil {
		return Item{}, fmt.Errorf("knowledge: supersede: %w", err)
	}

	return next, nil
}

// Retire marks a live item superseded by nothing in particular ("-"): it is
// no longer given to workers, and Why still finds it.
func (s *Store) Retire(id string) error {
	res, err := s.db.Exec(`UPDATE knowledge_items SET superseded_by = ? WHERE id = ? AND superseded_by = ''`, retired, id)
	if err != nil {
		return fmt.Errorf("knowledge: retire: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}

	return nil
}

// ForWorker returns the live items a worker is given: org first, then each of
// its roles in the order given, then its own - broad to narrow, so a narrower
// line read later can refine a broader one. Each group is oldest first.
func (s *Store) ForWorker(worker string, roles []string) ([]Item, error) {
	out := make([]Item, 0)

	org, err := s.live(ScopeOrg, "")
	if err != nil {
		return nil, err
	}
	out = append(out, org...)

	seen := map[string]bool{}
	for _, role := range roles {
		role = strings.TrimSpace(role)
		if role == "" || seen[role] {
			continue
		}
		seen[role] = true
		items, err := s.live(ScopeRole, role)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
	}

	if worker = strings.TrimSpace(worker); worker != "" {
		mine, err := s.live(ScopeWorker, worker)
		if err != nil {
			return nil, err
		}
		out = append(out, mine...)
	}

	return out, nil
}

// ForTask returns the live items scoped to one task, oldest first.
func (s *Store) ForTask(taskID string) ([]Item, error) {
	return s.live(ScopeTask, strings.TrimSpace(taskID))
}

// live returns the items of one scope and key that nothing superseded.
func (s *Store) live(scope, key string) ([]Item, error) {
	items, err := sqlitedb.QueryAll(s.db, scanItem,
		`SELECT `+columns+` FROM knowledge_items
		 WHERE scope = ? AND scope_key = ? AND superseded_by = ''
		 ORDER BY created_ns, id`, scope, key)
	if err != nil {
		return nil, fmt.Errorf("knowledge: list %s: %w", scope, err)
	}

	return items, nil
}

// Why returns an item and the chain of items it replaced, newest first: each
// carries its own source, so the chain shows who said what, and when, until
// the line read as it does now.
func (s *Store) Why(id string) (Item, []Item, error) {
	it, err := s.Get(id)
	if err != nil {
		return Item{}, nil, err
	}

	chain := make([]Item, 0)
	at := it.ID
	for len(chain) < maxChain {
		prev, err := scanItem(s.db.QueryRow(`SELECT `+columns+` FROM knowledge_items WHERE superseded_by = ?`, at))
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return Item{}, nil, fmt.Errorf("knowledge: why: %w", err)
		}
		chain = append(chain, prev)
		at = prev.ID
	}

	return it, chain, nil
}
