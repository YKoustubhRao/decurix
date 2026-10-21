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
)

// L1 is curated memory: the small set of things actually worth carrying
// across sessions, as opposed to L2's complete raw log of everything.
// An entry gets in one of two ways, both real judgment calls rather than
// blind capture:
//   - the user explicitly asks ("remember this", "my name is X")
//   - the model judges an exchange durably important on its own
//
// Real bug this exists to fix: "my name is Divyanshu, remember this"
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

	schema := `
CREATE TABLE IF NOT EXISTS facts (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	fact TEXT NOT NULL,
	source TEXT NOT NULL,
	embedding TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS facts_fact ON facts (fact);
`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("l1: create schema: %w", err)
	}
	return &L1{db: db, provider: provider, embedder: embedder, model: model}, nil
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
	}, llm.Options{TimeoutMs: 60000})
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
func (l *L1) Store(ctx context.Context, fact, source string) error {
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
