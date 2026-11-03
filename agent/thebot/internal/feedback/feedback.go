// Package feedback holds the two feedback RAGs from the design: positive
// (what the agent already does well) and negative (its own known
// limitations). This is the minimal real version - negative feedback
// only for now, since that's what the failure-reasoning loop actually
// consumes; positive feedback follows the identical pattern when needed.
//
// Two ways an entry gets written: the agent records one itself after a
// real, unresolved failure (self-generated), or a user gives explicit
// feedback that gets recorded directly. Either way, on a NEW failure, past
// negative feedback is consulted (semantic search) and threaded into the
// reasoning prompt - so a known limitation actually shapes behavior
// instead of being logged and forgotten.
package feedback

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	_ "modernc.org/sqlite"

	"thebot/internal/llm"
	"thebot/internal/memory"
)

// embedModel is deliberately empty - see rag.go's matching comment. Each
// llm.Embedder implementation owns its own default embed model.
const embedModel = ""

type Entry struct {
	Task      string
	Detail    string
	Source    string // "self" (agent's own failure) | "user" (explicit feedback)
	CreatedAt int64
	Score     float64
}

type Store struct {
	db       *sql.DB
	embedder llm.Embedder
}

func New(workspaceRoot string, embedder llm.Embedder) (*Store, error) {
	dir := filepath.Join(workspaceRoot, ".feedback")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("feedback: create dir: %w", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "negative.db"))
	if err != nil {
		return nil, fmt.Errorf("feedback: open db: %w", err)
	}
	db.SetMaxOpenConns(1)

	schema := `
CREATE TABLE IF NOT EXISTS negative_feedback (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	task TEXT NOT NULL,
	detail TEXT NOT NULL,
	source TEXT NOT NULL,
	embedding TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
`
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("feedback: create schema: %w", err)
	}
	if _, err := memory.PurgeSecrets(db, "negative_feedback", "task", "detail"); err != nil {
		db.Close()
		return nil, fmt.Errorf("feedback: purge secrets: %w", err)
	}
	return &Store{db: db, embedder: embedder}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// RecordNegative stores one real limitation - task is what was being
// attempted, detail is what went wrong / what the limitation is.
// An entry holding a secret-shaped value (memory.LooksSecret) is not kept:
// it would be recalled into later failures' reasoning prompts.
func (s *Store) RecordNegative(ctx context.Context, task, detail, source string) error {
	if memory.LooksSecret(task) || memory.LooksSecret(detail) {
		return memory.ErrSecret
	}
	vec, err := s.embedder.Embed(ctx, embedModel, task+"\n"+detail)
	if err != nil {
		return fmt.Errorf("feedback: embed entry: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO negative_feedback (task, detail, source, embedding, created_at) VALUES (?, ?, ?, ?, ?)`,
		task, detail, source, llm.EncodeVector(vec), time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("feedback: insert: %w", err)
	}
	return nil
}

// ConsultNegative returns past negative feedback relevant to the given
// task, most relevant first - real semantic search, never invented.
// Returns an empty slice (not an error) when nothing has been recorded
// yet, since "no feedback yet" is a normal state.
func (s *Store) ConsultNegative(ctx context.Context, task string, topK int) ([]Entry, error) {
	count, err := s.Count(ctx)
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, nil
	}

	queryVec, err := s.embedder.Embed(ctx, embedModel, task)
	if err != nil {
		return nil, fmt.Errorf("feedback: embed query: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, `SELECT task, detail, source, embedding, created_at FROM negative_feedback`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []Entry
	for rows.Next() {
		var e Entry
		var embStr string
		if err := rows.Scan(&e.Task, &e.Detail, &e.Source, &embStr, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Score = llm.CosineSimilarity(queryVec, llm.DecodeVector(embStr))
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Score > entries[j].Score })
	if len(entries) > topK {
		entries = entries[:topK]
	}
	return entries, nil
}

func (s *Store) Count(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM negative_feedback`).Scan(&n)
	return n, err
}

// ListRecent returns the most recent n real entries, newest first - no
// semantic search involved, just "what's actually in the store", for
// genuine transparency rather than requiring a query to see anything.
func (s *Store) ListRecent(ctx context.Context, n int) ([]Entry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT task, detail, source, created_at FROM negative_feedback
		ORDER BY created_at DESC LIMIT ?
	`, n)
	if err != nil {
		return nil, fmt.Errorf("feedback: list recent: %w", err)
	}
	defer rows.Close()

	var out []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.Task, &e.Detail, &e.Source, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("feedback: scan entry: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
