// Package trace records what Brainiac did for each request: which atoms ran
// on which models, how long each took, what came from the cache, what the
// brain was and what it cost in tokens, and what the checks found. Every
// answer carries its trace id, so "why did it say that" has an answer.
//
// Traces hold no content - no prompt text, no picture, no transcript - only
// sizes and timings, so they can be kept and shared.
//
// Depends on nothing internal.
package trace

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Atom is one small job inside a request.
type Atom struct {
	ID      string  `json:"id"`
	Op      string  `json:"op"` // ocr | describe | asr | brain
	Model   string  `json:"model,omitempty"`
	Runtime string  `json:"runtime,omitempty"`
	Ms      int64   `json:"ms"`
	Cached  bool    `json:"cached,omitempty"`
	OutSize int     `json:"out_chars,omitempty"`
	Unsure  int     `json:"unsure,omitempty"` // lines or segments read with low confidence
	Tokens  *Tokens `json:"tokens,omitempty"`
	Error   string  `json:"error,omitempty"`
	// LoadMs is time spent loading a model before the atom could run.
	LoadMs int64 `json:"load_ms,omitempty"`
}

// Tokens is a model call's usage.
type Tokens struct {
	Prompt     int  `json:"prompt"`
	Completion int  `json:"completion"`
	Cached     int  `json:"cached,omitempty"`
	Remote     bool `json:"remote,omitempty"`
}

// Record is one request.
type Record struct {
	ID    string    `json:"id"`
	At    time.Time `json:"at"`
	Model string    `json:"model"` // what the client asked for
	Mode  string    `json:"mode"`
	Brain string    `json:"brain"`
	// Domain is the field the request was routed to, with the router's
	// confidence (1 when the client named it).
	Domain  string   `json:"domain,omitempty"`
	DomainP float64  `json:"domain_p,omitempty"`
	Atoms   []Atom   `json:"atoms"`
	Ms      int64    `json:"ms"`
	Checks  []Check  `json:"checks,omitempty"`
	Notes   []string `json:"notes,omitempty"`
	Error   string   `json:"error,omitempty"`
	LocalT  int      `json:"local_tokens"`
	RemoteT int      `json:"remote_tokens"`
	// USD is what the remote brain's tokens cost, when its price is configured.
	USD float64 `json:"usd,omitempty"`
}

// Check is one thing the verifier found.
type Check struct {
	Kind   string   `json:"kind"`
	Detail string   `json:"detail"`
	Items  []string `json:"items,omitempty"`
}

// Store keeps recent records in memory and appends every one to a daily file.
type Store struct {
	Dir    string
	mu     sync.Mutex
	recent []*Record
	byID   map[string]*Record
}

// NewStore keeps traces under dir.
func NewStore(dir string) *Store { return &Store{Dir: dir, byID: map[string]*Record{}} }

// NewID is a short random id for a request.
func NewID() string { return fmt.Sprintf("%x", rand.Uint64()) }

// Add files a finished record.
func (s *Store) Add(r *Record) {
	if s == nil || r == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recent = append(s.recent, r)
	s.byID[r.ID] = r
	if len(s.recent) > 200 {
		delete(s.byID, s.recent[0].ID)
		s.recent = s.recent[1:]
	}
	if s.Dir == "" {
		return
	}
	if os.MkdirAll(s.Dir, 0o755) != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(s.Dir, r.At.UTC().Format("2006-01-02")+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if data, err := json.Marshal(r); err == nil {
		f.Write(append(data, '\n'))
	}
}

// Get returns a recent record by id.
func (s *Store) Get(id string) (*Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.byID[id]
	return r, ok
}

// Recent returns up to n of the newest records, newest first.
func (s *Store) Recent(n int) []*Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Record
	for i := len(s.recent) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, s.recent[i])
	}
	return out
}
