// Package suggest picks a desktop for a request that has not been sent yet:
// the one most recently actually in use, or - when some desktop's own
// worker memory (internal/memory) closely matches what the request
// describes - that desktop instead, found by embedding text through the
// central Ollama (internal/ollama) and comparing by cosine similarity.
//
// Every failure path - no Ollama, no embedding model, nothing in memory yet
// - just means no semantic match and a fall back to recency. This must
// never block or fail sending a request, the same best-effort posture
// internal/ollama's own preloadModel already takes.
package suggest

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/memory"
	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// DesktopLister is what this package needs from desktop.Manager: list
// desktops, and say when one was last actually looked at (its thumbnail
// fetched, or its screen proxied to). Satisfied by *desktop.Manager;
// narrowed to an interface for the same reason internal/idlepause narrows
// the same two calls - so a fake can stand in for tests that have no Docker
// daemon to talk to.
type DesktopLister interface {
	List(ctx context.Context) ([]desktop.Desktop, error)
	LastViewed(id string) (time.Time, bool)
}

// ActivityStore is what this package needs from tasks.Store: when a
// worker's task records last changed. Satisfied by *tasks.Store.
type ActivityStore interface {
	LastActivity(workerID string) (time.Time, bool, error)
}

// MemoryLister is what this package needs from memory.Store: every recorded
// worker-memory entry. Satisfied by *memory.Store.
type MemoryLister interface {
	List(desktopID string) []memory.WorkerMemoryEntry
}

// Embedder turns text into a vector. ollama.Embed satisfies it; narrowed to
// a function type so tests don't need a real Ollama.
type Embedder func(ctx context.Context, text string) ([]float32, error)

// minContentChars is how much typed text is worth embedding at all - a
// two-word request has too little signal to match against, and is not
// worth an Ollama round trip.
const minContentChars = 8

// minConfidence is the cosine similarity a memory match must clear to be
// suggested over the recency fallback. 0.55 is a starting guess for
// nomic-embed-text, not a measured value - there is no fleet with real
// memory entries yet to tune it against. Revisit once there is.
const minConfidence = 0.55

// Suggestion is the desktop Store.Suggest recommends for a request, and why.
type Suggestion struct {
	DesktopID   string  `json:"desktop_id"`
	DesktopName string  `json:"desktop_name"`
	Reason      string  `json:"reason"` // "memory" or "recent"
	Why         string  `json:"why"`
	Score       float64 `json:"score,omitempty"`
}

var schema = []string{
	`CREATE TABLE suggestion_embeddings (
		memory_id  TEXT PRIMARY KEY,
		vector     BLOB NOT NULL,
		dims       INTEGER NOT NULL,
		created_ns INTEGER NOT NULL
	)`,
}

// Store picks a desktop for a typed request. See the package doc for the
// recency/memory fallback rule.
type Store struct {
	db       *sql.DB
	Desktops DesktopLister
	Tasks    ActivityStore
	Memory   MemoryLister
	Embed    Embedder
}

// NewStore creates a Store, creating its embedding cache table if needed.
func NewStore(db *sql.DB, desktops DesktopLister, tasksStore ActivityStore, memoryStore MemoryLister, embed Embedder) (*Store, error) {
	if err := sqlitedb.Migrate(db, "suggest", schema); err != nil {
		return nil, err
	}

	return &Store{db: db, Desktops: desktops, Tasks: tasksStore, Memory: memoryStore, Embed: embed}, nil
}

// Suggest recommends a desktop for content, or reports false when there is
// nothing to suggest (no desktops at all).
func (s *Store) Suggest(ctx context.Context, content string) (Suggestion, bool) {
	content = strings.TrimSpace(content)

	desktops, err := s.Desktops.List(ctx)
	if err != nil || len(desktops) == 0 {
		return Suggestion{}, false
	}

	if len(content) >= minContentChars && s.Embed != nil {
		if match, found := s.memoryPick(ctx, content, desktops); found {
			return match, true
		}
	}

	return s.recencyPick(desktops), true
}

// recencyPick returns the desktop most recently actually touched, among
// desktops (which must be non-empty).
func (s *Store) recencyPick(desktops []desktop.Desktop) Suggestion {
	best := desktops[0]
	bestSince := s.lastActive(best)

	for _, d := range desktops[1:] {
		if since := s.lastActive(d); since.After(bestSince) {
			best, bestSince = d, since
		}
	}

	return Suggestion{DesktopID: best.ID, DesktopName: best.Name, Reason: "recent", Why: "most recently active"}
}

// lastActive mirrors idlepause.Checker.idleSince's "latest of every signal
// we have" rule, aimed the other way: not "how long has this sat idle" but
// "when was this last actually touched." Falls back to CreatedAt when none
// of the other three signals are available at all, so it always returns
// something comparable rather than the zero time winning by default.
func (s *Store) lastActive(d desktop.Desktop) time.Time {
	since, ok := parseTimestamp(d.StartedAt)

	if last, found, err := s.Tasks.LastActivity(d.ID); err == nil && found && (!ok || last.After(since)) {
		since, ok = last, true
	}

	if last, found := s.Desktops.LastViewed(d.ID); found && (!ok || last.After(since)) {
		since, ok = last, true
	}

	if !ok {
		since, _ = parseTimestamp(d.CreatedAt)
	}

	return since
}

// parseTimestamp reads an RFC3339(Nano) timestamp, treating both a parse
// failure and a zero-value "never happened" timestamp as no signal.
func parseTimestamp(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}

	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || t.Year() <= 1 {
		return time.Time{}, false
	}

	return t, true
}

// memoryPick looks for a worker-memory entry, belonging to one of desktops,
// whose embedding is close enough to content's to suggest that desktop
// instead of the recency fallback.
func (s *Store) memoryPick(ctx context.Context, content string, desktops []desktop.Desktop) (Suggestion, bool) {
	candidates := make(map[string]desktop.Desktop, len(desktops))
	for _, d := range desktops {
		candidates[d.ID] = d
	}

	// Scoped to entries that could actually be suggested, not every memory
	// entry the whole fleet has ever recorded (memory.Store.List(desktopID)
	// filters in SQL, but the "every desktop" call this package needs has
	// no such filter to give it - see MemoryLister). This is what keeps
	// both the embedding backfill below and the cache query it feeds from
	// growing with total fleet memory instead of with what this particular
	// request could ever match.
	entries := make([]memory.WorkerMemoryEntry, 0)
	for _, entry := range s.Memory.List("") {
		if _, ok := candidates[entry.DesktopID]; ok {
			entries = append(entries, entry)
		}
	}

	if len(entries) == 0 {
		return Suggestion{}, false
	}

	// The request text's own embedding first: it is the one call that
	// actually decides this request's outcome. Backfilling missing cache
	// entries (below) is opportunistic upkeep - it must never be allowed to
	// spend this call's whole ctx deadline before the call that matters
	// even runs.
	contentVector, err := s.Embed(ctx, content)
	if err != nil {
		log.Printf("suggest: embedding request text: %v", err)
		return Suggestion{}, false
	}
	contentVector = normalizeVector(contentVector)

	vectors := s.loadCachedVectors(entries)
	s.cacheMissingVectors(ctx, entries, vectors)

	var best memory.WorkerMemoryEntry
	bestScore := minConfidence

	for _, entry := range entries {
		vector, ok := vectors[entry.ID]
		if !ok {
			continue
		}

		if score := cosineSimilarity(contentVector, vector); score >= bestScore {
			best, bestScore = entry, score
		}
	}

	if best.ID == "" {
		return Suggestion{}, false
	}

	d := candidates[best.DesktopID]

	return Suggestion{
		DesktopID:   d.ID,
		DesktopName: d.Name,
		Reason:      "memory",
		Why:         fmt.Sprintf("similar memory: %q", best.Title),
		Score:       bestScore,
	}, true
}

// loadCachedVectors reads the cached embedding for each of entries (never
// the whole suggestion_embeddings table - that would grow with total fleet
// memory rather than with what this call could ever use). Rows are fully
// read and closed before returning - the SQLite pool here is one
// connection, and no query may run while a *sql.Rows from an earlier one is
// still open.
func (s *Store) loadCachedVectors(entries []memory.WorkerMemoryEntry) map[string][]float32 {
	vectors := make(map[string][]float32, len(entries))
	if len(entries) == 0 {
		return vectors
	}

	placeholders := make([]string, len(entries))
	args := make([]any, len(entries))

	for i, entry := range entries {
		placeholders[i] = "?"
		args[i] = entry.ID
	}

	query := `SELECT memory_id, vector, dims FROM suggestion_embeddings WHERE memory_id IN (` +
		strings.Join(placeholders, ",") + `)`

	rows, err := s.db.Query(query, args...)
	if err != nil {
		log.Printf("suggest: loading cached embeddings: %v", err)
		return vectors
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		var raw []byte
		var dims int

		if err := rows.Scan(&id, &raw, &dims); err != nil {
			log.Printf("suggest: scanning cached embedding: %v", err)
			continue
		}

		vectors[id] = normalizeVector(decodeVector(raw, dims))
	}

	return vectors
}

// maxBackfillPerCall bounds how many missing embeddings one Suggest call
// will attempt before leaving the rest for a later call. A cold cache (or a
// burst of newly recorded memory) would otherwise turn a single compose-form
// suggestion into dozens of sequential Ollama round trips inside the
// handler's own ctx budget (internal/httpapi's suggestTimeout) - the cache
// still only ever grows, one call's leftovers are just picked up by the
// next.
const maxBackfillPerCall = 20

// cacheMissingVectors embeds and persists up to maxBackfillPerCall entries
// not already in vectors, adding each one to vectors as it succeeds.
// Best-effort per entry: one embedding failure is logged and skipped, never
// aborts the rest.
func (s *Store) cacheMissingVectors(ctx context.Context, entries []memory.WorkerMemoryEntry, vectors map[string][]float32) {
	attempted := 0

	for _, entry := range entries {
		if _, ok := vectors[entry.ID]; ok {
			continue
		}

		if attempted >= maxBackfillPerCall {
			return
		}
		attempted++

		vector, err := s.Embed(ctx, entry.Title+"\n"+entry.Summary)
		if err != nil {
			log.Printf("suggest: embedding memory entry %s: %v", entry.ID, err)
			continue
		}

		if _, err := s.db.Exec(
			`INSERT OR IGNORE INTO suggestion_embeddings (memory_id, vector, dims, created_ns) VALUES (?, ?, ?, ?)`,
			entry.ID, encodeVector(vector), len(vector), sqlitedb.ToNS(time.Now()),
		); err != nil {
			log.Printf("suggest: caching embedding for memory entry %s: %v", entry.ID, err)
			continue
		}

		vectors[entry.ID] = normalizeVector(vector)
	}
}

// encodeVector packs a vector as little-endian float32 bytes.
func encodeVector(v []float32) []byte {
	buf := make([]byte, len(v)*4)

	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}

	return buf
}

// decodeVector unpacks a vector encoded by encodeVector. A short buffer
// (dims recorded larger than the bytes actually available) yields zeros
// for the missing tail rather than panicking.
func decodeVector(raw []byte, dims int) []float32 {
	v := make([]float32, dims)

	for i := range v {
		if (i+1)*4 > len(raw) {
			break
		}

		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}

	return v
}

// normalizeVector returns a unit vector (norm = 1.0) for fast dot-product cosine similarity.
// If v is empty or zero, it returns v unchanged.
func normalizeVector(v []float32) []float32 {
	if len(v) == 0 {
		return v
	}
	var norm float64
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	if norm == 0 {
		return v
	}
	inv := float32(1.0 / math.Sqrt(norm))
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x * inv
	}
	return out
}

// cosineSimilarity returns the cosine similarity of a and b, or 0 when they
// are empty, mismatched in length, or either is the zero vector.
// When both a and b are unit-normalized vectors, this evaluates directly as an inner dot product.
func cosineSimilarity(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}

	var dot, normA, normB float64

	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}

	// Fast path for normalized unit vectors
	if math.Abs(normA-1.0) < 1e-4 && math.Abs(normB-1.0) < 1e-4 {
		return dot
	}

	if normA == 0 || normB == 0 {
		return 0
	}

	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}
