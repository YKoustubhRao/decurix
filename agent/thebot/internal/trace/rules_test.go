// Tests for what the trace keeps beside its lines and across a restart: a trace at its cap stays stopped when soul
// restarts (no second truncated line), and the rules a task's job was given are noted by hash only.
package trace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Each restart of soul on a full trace used to add one more truncated line: a writer that opens a file ending with
// it is stopped from the start.
func TestARestartOnAFullTraceAddsNoSecondTruncatedLine(t *testing.T) {
	dir := t.TempDir()
	w := New(dir)
	w.max = 4096
	for i := 0; i < 50; i++ {
		w.CommandRun(taskCtx(w), "echo hello", GuardAllowed, nil, 0, strings.Repeat("y", 300), nil)
	}
	path := filepath.Join(dir, "t_9.jsonl")
	for restart := 0; restart < 3; restart++ {
		again := New(dir) // the default cap: room to spare, so only the stopped state keeps the file as it is
		again.CommandRun(taskCtx(again), "echo more", GuardAllowed, nil, 0, "", nil)
	}
	n := 0
	for _, l := range readLines(t, path) {
		if l["kind"] == "truncated" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d truncated lines after three restarts, want 1", n)
	}
	lines := readLines(t, path)
	if lines[len(lines)-1]["kind"] != "truncated" {
		t.Fatal("a restarted writer wrote past the truncated line")
	}
}

// A file that does not end with the truncated line goes on after a restart.
func TestARestartOnATraceUnderItsCapGoesOn(t *testing.T) {
	dir := t.TempDir()
	New(dir).CommandRun(taskCtx(New(dir)), "echo one", GuardAllowed, nil, 0, "", nil)
	again := New(dir)
	again.CommandRun(taskCtx(again), "echo two", GuardAllowed, nil, 0, "", nil)
	if lines := readLines(t, filepath.Join(dir, "t_9.jsonl")); len(lines) != 2 {
		t.Fatalf("%d lines, want 2", len(lines))
	}
}

// The rules a job was given are noted as their SHA-256 only, replaced by the next job's, and never for a call with
// no task.
func TestTheRulesAreNotedByHashOnly(t *testing.T) {
	dir := t.TempDir()
	w := New(dir)
	w.Rules(taskCtx(w), "Rule one: never pay without a purchase order.")
	path := filepath.Join(dir, "t_9.rules")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("Rule one: never pay without a purchase order."))
	if string(b) != "sha256:"+hex.EncodeToString(sum[:])+"\n" || strings.Contains(string(b), "purchase") {
		t.Fatalf("rules file %q", b)
	}
	w.Rules(taskCtx(w), "Rule two.")
	b, _ = os.ReadFile(path)
	sum = sha256.Sum256([]byte("Rule two."))
	if string(b) != "sha256:"+hex.EncodeToString(sum[:])+"\n" {
		t.Fatalf("the next job's rules did not replace the file: %q", b)
	}
	New(dir).Rules(WithWriter(context.Background(), w), "no task")
	(*Writer)(nil).Rules(taskCtx(w), "nil writer")
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("files in the trace folder: %d, want only t_9.rules", len(entries))
	}
}
