package memory

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"thebot/internal/llm"
	"thebot/internal/migrate"
)

// L1 is curated memory: the small set of things actually worth carrying
// across sessions, as opposed to L2's complete raw log of everything.
// An entry gets in one of two ways, both real judgment calls rather than
// blind capture:
//   - the user explicitly asks ("remember this", "my name is X")
//   - the model judges an exchange durably important on its own
//
// Real bug this exists to fix: "my name is Alex, remember this"
// produced a cheerful "of course I'll remember!" and stored absolutely
// nothing, because no L1 existed at all. The model wasn't lying so much
// as the system never gave it anywhere to put it.
type L1 struct {
	storeMu  sync.Mutex // Serializes duplicate detection and the following write.
	db       *sql.DB
	provider llm.Provider
	embedder llm.Embedder
	model    string
}

type L1Entry struct {
	Fact      string
	Source    string // "explicit" (user asked) | "inferred" (model judged)
	CreatedAt int64
	Score     float64
}

func NewL1(workspaceRoot string, provider llm.Provider, embedder llm.Embedder, model string) (*L1, error) {
	dir := filepath.Join(workspaceRoot, ".memory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("l1: create dir: %w", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "l1.db"))
	if err != nil {
		return nil, fmt.Errorf("l1: open db: %w", err)
	}
	db.SetMaxOpenConns(1)

	if err := migrate.Migrate(db, L1Migrate); err != nil {
		db.Close()
		return nil, fmt.Errorf("l1: %w", err)
	}
	if _, err := PurgeSecrets(db, "facts", "fact"); err != nil {
		db.Close()
		return nil, fmt.Errorf("l1: purge secrets: %w", err)
	}
	return &L1{db: db, provider: provider, embedder: embedder, model: model}, nil
}

// L1Migrate is l1.db's schema, step by step: appended only, never edited (internal/migrate).
var L1Migrate = []string{
	`CREATE TABLE IF NOT EXISTS facts (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	fact TEXT NOT NULL,
	source TEXT NOT NULL,
	embedding TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS facts_fact ON facts (fact);`,
	fixesSchema,
}

// PurgeSecrets deletes every row of table whose columns hold a secret-shaped value (LooksSecret) and returns how
// many it deleted. A store opened by an older soul, whose check let a secret in, is cleaned when it is opened: L1 and
// the feedback store are recalled into later prompts, so a secret kept there leaks into every one of them. The ids
// are read first and the rows deleted after the read is closed (no query while a *sql.Rows is open).
func PurgeSecrets(db *sql.DB, table string, columns ...string) (int, error) {
	rows, err := db.Query(`SELECT id, ` + strings.Join(columns, ` || char(10) || `) + ` FROM ` + table)
	if err != nil {
		return 0, err
	}
	var doomed []int64
	for rows.Next() {
		var id int64
		var text string
		if err := rows.Scan(&id, &text); err != nil {
			rows.Close()
			return 0, err
		}
		if LooksSecret(text) {
			doomed = append(doomed, id)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	for _, id := range doomed {
		if _, err := db.Exec(`DELETE FROM `+table+` WHERE id = ?`, id); err != nil {
			return 0, err
		}
	}
	return len(doomed), nil
}

func (l *L1) Close() error { return l.db.Close() }

const judgePrompt = `Decide whether this exchange reveals a DURABLE fact about the USER worth remembering in future conversations.

Remember ONLY things like:
- who the user is (name, role, company)
- their stated preferences and how they want things done
- long-lived project facts and decisions they made
- anything they explicitly asked you to remember

Do NOT remember:
- what task they asked for this time ("the user asked me to write a file") - that is task history, not a fact about them
- small talk, greetings, arithmetic, or trivia answers
- anything about what YOU did or produced
- restatements of something already obvious from the request

If in doubt, answer no. A wrong "yes" pollutes memory permanently.

User said:
%s

You answered:
%s

Respond in EXACTLY this format, nothing else:
REMEMBER: yes or no
FACT: <if yes, the single durable fact about the user, in the third person, e.g. "The user's name is Alex Doe." If no, leave blank>`

// ConsiderExchange makes one real judgment call about whether an exchange
// is worth keeping, and stores it if so. Fire-and-forget by design: never
// blocks the answer the user is waiting on, and a failure here is logged
// rather than surfaced as a task failure.
func (l *L1) ConsiderExchange(ctx context.Context, prompt, answer string, onLog func(string)) {
	log := func(msg string) {
		if onLog != nil {
			onLog("l1: " + msg)
		}
	}

	resp, err := l.provider.Chat(ctx, l.model, []llm.Message{
		{Role: "user", Content: fmt.Sprintf(judgePrompt, prompt, answer)},
	}, llm.Options{TimeoutMs: 60000, Purpose: llm.PurposeMemoryL1})
	if err != nil {
		log("could not judge whether this is worth remembering: " + err.Error())
		return
	}

	decision, fact := parseJudgement(resp.Text)
	if !decision || strings.TrimSpace(fact) == "" {
		log("nothing here worth storing long-term")
		return
	}

	source := "inferred"
	if looksExplicit(prompt) {
		source = "explicit"
	}
	// A fact about the person holds only values the person said. One the
	// answer carried - a client's GSTIN from an invoice, a total from a
	// sheet, a number a page printed - belongs to that task's documents, and
	// L1 has no scope to keep it in: it would be recalled into every later
	// task as if it were known about the person.
	if unsaid := unsaidValues(fact, prompt); len(unsaid) > 0 {
		log(fmt.Sprintf("not stored: %s (it carries %d value(s) the person never said)", ErrUnscoped.Error(), len(unsaid)))
		return
	}

	if err := l.Store(ctx, fact, source); err != nil {
		log("failed to store: " + err.Error())
		return
	}
	log("stored: " + fact)
}

// memorableRe recognises a turn in which the human says something about
// themselves or asks to be remembered - the only turns L1's judge can ever
// answer "yes" for, by its own prompt's rules.
var memorableRe = regexp.MustCompile(`(?i)\b(remember|don'?t forget|keep in mind|note that|from now on|going forward|call me|my name|who i am)\b` +
	`|\bmy\s+(job|role|title|company|employer|team|boss|manager|email|phone|number|birthday|wife|husband|partner|girlfriend|boyfriend|kids?|children|son|daughter|dog|cat|favou?rite|preferences?|timezone|time zone|city|country|address|language|pronouns|project|startup|business)\b` +
	`|\b(i am|i'm)\s+(a|an|the|from|based|working|called|named)\b` +
	`|\bi\s+(work|live|prefer|like|love|hate|always|never|usually|use|want you to)\b`)

// LooksMemorable reports whether a human's turn could carry a durable fact
// about them. It gates ConsiderExchange, which costs a whole model call:
// most turns ("check the disk space", "what's the kernel version?") could
// never produce a memory, and judging them anyway spent that call - and
// evicted the model's cached prompt - after every single task.
func LooksMemorable(prompt string) bool {
	return memorableRe.MatchString(prompt)
}

// looksExplicit reports whether the user actually asked for this to be
// remembered, versus the model deciding on its own - recorded so the two
// are distinguishable later rather than blended together.
func looksExplicit(prompt string) bool {
	p := strings.ToLower(prompt)
	for _, marker := range []string{"remember", "don't forget", "dont forget", "keep in mind", "note that", "my name is"} {
		if strings.Contains(p, marker) {
			return true
		}
	}
	return false
}

func parseJudgement(text string) (bool, string) {
	var remember bool
	var fact string
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "remember:") {
			remember = strings.Contains(lower, "yes")
		}
		if strings.HasPrefix(lower, "fact:") {
			fact = strings.TrimSpace(trimmed[len("fact:"):])
		}
	}
	return remember, fact
}

// dedupeThreshold is how similar a new fact must be to an existing one to
// count as already known. Found live: asking "what's my name" right after
// being told it stored "The user's name is X" a SECOND time, because the
// answer restated the fact and the judge (correctly) called it important
// again. Near-duplicates crowd out real recall slots, so they're merged.
const dedupeThreshold = 0.95

// Store writes one real fact with its real embedding, unless L1 already
// holds something that means the same thing.
//
// A fact holding a secret-shaped value (LooksSecret: a password, PIN, OTP,
// token or key) is never stored, whoever asks: L1 is recalled into every
// later job's system frame, and a secret there is in every prompt after it.
func (l *L1) Store(ctx context.Context, fact, source string) error {
	if LooksSecret(fact) {
		return ErrSecret
	}
	l.storeMu.Lock()
	defer l.storeMu.Unlock()

	// Exact repeats need neither a model round trip nor a vector-table scan.
	result, err := l.db.ExecContext(ctx,
		`UPDATE facts SET created_at = ?, source = CASE WHEN source = 'explicit' THEN 'explicit' ELSE ? END WHERE fact = ?`,
		time.Now().Unix(), source, fact)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n > 0 {
		return nil
	}

	vec, err := l.embedder.Embed(ctx, "", fact)
	if err != nil {
		return fmt.Errorf("l1: embed fact: %w", err)
	}

	rows, err := l.db.QueryContext(ctx, `SELECT id, embedding FROM facts`)
	if err != nil {
		return err
	}
	dupeID := int64(-1)
	for rows.Next() {
		var id int64
		var embStr string
		if err := rows.Scan(&id, &embStr); err != nil {
			rows.Close()
			return err
		}
		if llm.CosineSimilarity(vec, llm.DecodeVector(embStr)) >= dedupeThreshold {
			dupeID = id
			break
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	if dupeID >= 0 {
		// Refresh the existing entry rather than adding a second copy -
		// an explicit restatement should also upgrade an inferred one.
		_, err = l.db.ExecContext(ctx,
			`UPDATE facts SET fact = ?, embedding = ?, created_at = ?, source = CASE WHEN source = 'explicit' THEN 'explicit' ELSE ? END WHERE id = ?`,
			fact, llm.EncodeVector(vec), time.Now().Unix(), source, dupeID)
		return err
	}

	_, err = l.db.ExecContext(ctx,
		`INSERT INTO facts (fact, source, embedding, created_at) VALUES (?, ?, ?, ?)`,
		fact, source, llm.EncodeVector(vec), time.Now().Unix(),
	)
	return err
}

// Recall returns the facts most relevant to a query, most relevant first.
// Returns nothing (not an error) when L1 is empty - that's a normal state,
// not a failure.
//
// No score floor (review T-19, measured 2026-11-03 on nomic-embed-text as soul calls it): a fact that answers the
// query scored 0.44-0.69 and an unrelated one 0.28-0.56, so any fixed floor either drops real facts or keeps
// unrelated ones. What a recalled fact is worth is said where it is shown instead (prompt.Frame: "may be relevant").
func (l *L1) Recall(ctx context.Context, query string, topK int) ([]L1Entry, error) {
	if topK <= 0 {
		return nil, nil
	}
	count, err := l.Count(ctx)
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, nil
	}

	queryVec, err := l.embedder.Embed(ctx, "", query)
	if err != nil {
		return nil, fmt.Errorf("l1: embed query: %w", err)
	}

	rows, err := l.db.QueryContext(ctx, `SELECT fact, source, embedding, created_at FROM facts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []L1Entry
	for rows.Next() {
		var e L1Entry
		var embStr string
		if err := rows.Scan(&e.Fact, &e.Source, &embStr, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Score = llm.CosineSimilarity(queryVec, llm.DecodeVector(embStr))
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > topK {
		out = out[:topK]
	}
	return out, nil
}

// Count is the real number of facts held.
func (l *L1) Count(ctx context.Context) (int, error) {
	var n int
	err := l.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM facts`).Scan(&n)
	return n, err
}

// ListRecent returns the most recently stored facts, newest first.
func (l *L1) ListRecent(ctx context.Context, n int) ([]L1Entry, error) {
	rows, err := l.db.QueryContext(ctx,
		`SELECT fact, source, created_at FROM facts ORDER BY created_at DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []L1Entry
	for rows.Next() {
		var e L1Entry
		if err := rows.Scan(&e.Fact, &e.Source, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
