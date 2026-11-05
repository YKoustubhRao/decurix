// Package memory holds TheBot's memory tiers. L2 (this file) is the
// complete raw log - every job, unfiltered, append-only. L1 (curated,
// judged-important facts), session memory, and the feedback RAGs are
// later phases - not stubbed here, since a hollow stub is worse than an
// honestly-absent package.
package memory

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type L2Entry struct {
	Timestamp string   `json:"timestamp"`
	Prompt    string   `json:"prompt"`
	Status    string   `json:"status"`
	Plan      []string `json:"plan,omitempty"`
}

// L2 is append-only JSONL at <workspaceRoot>/.memory/l2.jsonl - mirrors
// the Node prototype's L2 exactly: the ground truth of everything that
// ever happened, never summarized. A job's secret values are withheld from
// what it keeps (scheduler's l2Entry, PLAN-P2 C14).
type L2 struct {
	path string
	mu   sync.Mutex
}

func NewL2(workspaceRoot string) (*L2, error) {
	dir := filepath.Join(workspaceRoot, ".memory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &L2{path: filepath.Join(dir, "l2.jsonl")}, nil
}

func (l *L2) Append(entry L2Entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if entry.Timestamp == "" {
		entry.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// readAll parses every real line in the log - the single source both
// Count and Recent build on, so "how many jobs happened" and "what were
// the last N" can never disagree.
func (l *L2) readAll() ([]L2Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	f, err := os.Open(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var entries []L2Entry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var e L2Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // one corrupted line shouldn't hide every other real entry
		}
		entries = append(entries, e)
	}
	return entries, scanner.Err()
}

// Count is the real number of jobs ever logged - not an estimate.
func (l *L2) Count() (int, error) {
	entries, err := l.readAll()
	return len(entries), err
}

// Recent returns the most recent n real entries, newest first.
func (l *L2) Recent(n int) ([]L2Entry, error) {
	entries, err := l.readAll()
	if err != nil {
		return nil, err
	}
	if len(entries) > n {
		entries = entries[len(entries)-n:]
	}
	// newest first - reverse in place
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	return entries, nil
}
