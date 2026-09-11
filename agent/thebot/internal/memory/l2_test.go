package memory

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestL2AppendWritesRealFile(t *testing.T) {
	root := t.TempDir()
	l2, err := NewL2(root)
	if err != nil {
		t.Fatalf("NewL2() error = %v", err)
	}

	if err := l2.Append(L2Entry{Prompt: "first", Status: "done"}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if err := l2.Append(L2Entry{Prompt: "second", Status: "error", Plan: []string{"step1", "step2"}}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	path := filepath.Join(root, ".memory", "l2.jsonl")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("expected log file at %s, got error: %v", path, err)
	}
	defer f.Close()

	var entries []L2Entry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var e L2Entry
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			t.Fatalf("line is not valid JSON: %v (%s)", err, scanner.Text())
		}
		entries = append(entries, e)
	}

	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0].Prompt != "first" || entries[0].Status != "done" {
		t.Errorf("entry 0 = %+v, unexpected", entries[0])
	}
	if entries[1].Prompt != "second" || entries[1].Status != "error" || len(entries[1].Plan) != 2 {
		t.Errorf("entry 1 = %+v, unexpected", entries[1])
	}
	if entries[0].Timestamp == "" {
		t.Errorf("entry 0 timestamp was not auto-filled")
	}
}

func TestL2CountOnEmptyLog(t *testing.T) {
	l2, err := NewL2(t.TempDir())
	if err != nil {
		t.Fatalf("NewL2() error = %v", err)
	}
	n, err := l2.Count()
	if err != nil {
		t.Fatalf("Count() error = %v", err)
	}
	if n != 0 {
		t.Errorf("Count() on a fresh log = %d, want 0", n)
	}
}

func TestL2CountAndRecent(t *testing.T) {
	l2, err := NewL2(t.TempDir())
	if err != nil {
		t.Fatalf("NewL2() error = %v", err)
	}
	for _, p := range []string{"first", "second", "third"} {
		if err := l2.Append(L2Entry{Prompt: p, Status: "done"}); err != nil {
			t.Fatalf("Append(%q) error = %v", p, err)
		}
	}

	n, err := l2.Count()
	if err != nil {
		t.Fatalf("Count() error = %v", err)
	}
	if n != 3 {
		t.Errorf("Count() = %d, want 3", n)
	}

	recent, err := l2.Recent(2)
	if err != nil {
		t.Fatalf("Recent(2) error = %v", err)
	}
	if len(recent) != 2 {
		t.Fatalf("Recent(2) returned %d entries, want 2", len(recent))
	}
	if recent[0].Prompt != "third" || recent[1].Prompt != "second" {
		t.Errorf("Recent(2) = [%q, %q], want [\"third\", \"second\"] (newest first)", recent[0].Prompt, recent[1].Prompt)
	}

	all, err := l2.Recent(100)
	if err != nil {
		t.Fatalf("Recent(100) error = %v", err)
	}
	if len(all) != 3 {
		t.Errorf("Recent(100) with only 3 real entries = %d, want 3 (not padded/truncated wrongly)", len(all))
	}
}
