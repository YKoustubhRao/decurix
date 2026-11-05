package trace

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"thebot/internal/llm"
	"thebot/internal/withheld"
)

// readLines is every line of a trace file, decoded.
func readLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("a trace line is not JSON: %v: %s", err, sc.Text())
		}
		out = append(out, m)
	}
	return out
}

type answering struct{ err error }

func (a answering) Name() string                     { return "openai" }
func (a answering) IsReachable(context.Context) bool { return true }
func (a answering) Chat(context.Context, string, []llm.Message, llm.Options) (llm.Result, error) {
	return llm.Result{Text: "ok", PromptTokens: 30, CachedTokens: 20, ResponseTokens: 4}, a.err
}

func taskCtx(w *Writer) context.Context {
	return WithWriter(llm.NextTurn(llm.WithTags(context.Background(), llm.Tags{Task: "t_9", Job: "j1"})), w)
}

func TestACallLeavesOneLineWithItsHashAndUsage(t *testing.T) {
	dir := t.TempDir()
	w := New(dir)
	p := Calls(answering{})
	msgs := []llm.Message{{Role: "system", Content: "s"}, {Role: "user", Content: "u"}}
	if _, err := p.Chat(taskCtx(w), "default", msgs, llm.Options{Purpose: llm.PurposeOperatorTurn}); err != nil {
		t.Fatal(err)
	}
	// A failed call is traced too.
	if _, err := Calls(answering{err: errors.New("down")}).Chat(taskCtx(w), "default", msgs, llm.Options{Purpose: llm.PurposeReview}); err == nil {
		t.Fatal("the error was swallowed")
	}
	lines := readLines(t, filepath.Join(dir, "t_9.jsonl"))
	if len(lines) != 2 {
		t.Fatalf("lines = %v", lines)
	}
	l := lines[0]
	if l["schema"] != Schema || l["kind"] != "call" || l["task"] != "t_9" || l["job"] != "j1" || l["turn"] != "j1:1" ||
		l["purpose"] != "operator.turn" || l["provider"] != "openai" || l["model"] != "default" || l["messages"] != 2.0 {
		t.Fatalf("call line = %v", l)
	}
	if u := l["usage"].(map[string]any); u["in"] != 30.0 || u["cached"] != 20.0 || u["out"] != 4.0 {
		t.Fatalf("usage = %v", u)
	}
	if sha, _ := l["prompt_sha256"].(string); len(sha) != 64 {
		t.Fatalf("prompt_sha256 = %v", l["prompt_sha256"])
	}
	if l["command"] != nil {
		t.Fatalf("a call line carries a command: %v", l)
	}
	if lines[1]["failed"] != true || lines[1]["purpose"] != "review" {
		t.Fatalf("failed call line = %v", lines[1])
	}
	if strings.Contains(mustRead(t, filepath.Join(dir, "t_9.jsonl")), `"content"`) {
		t.Fatal("a message body reached the trace")
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestNoTaskNoTrace(t *testing.T) {
	dir := t.TempDir()
	w := New(dir)
	ctx := WithWriter(llm.WithTags(context.Background(), llm.Tags{Job: "j"}), w)
	if _, err := Calls(answering{}).Chat(ctx, "m", nil, llm.Options{Purpose: llm.PurposeGeneral}); err != nil {
		t.Fatal(err)
	}
	w.CommandRun(ctx, "ls", GuardAllowed, nil, 0, "", nil)
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("a trace was written with no task: %v", entries)
	}
	// And a nil writer is fine everywhere.
	var none *Writer
	none.CommandRun(ctx, "ls", GuardAllowed, nil, 0, "", nil)
	none.Call(ctx, "", "", "", nil, llm.Result{}, 0, false)
}

func TestACommandLineIsRedacted(t *testing.T) {
	dir := t.TempDir()
	w := New(dir)
	exit := 0
	output := "logged in\npassword: Northwind#2026\n" + strings.Repeat("x", 3000)
	clean := func(s string) string { return strings.ReplaceAll(s, "s3cr3tvalue", "the password (not shown here)") }
	w.CommandRun(taskCtx(w), "gui type e3 's3cr3tvalue' && echo token: ghp_abcdefghijklmnopqrstuvwxyz0123", GuardAllowed,
		&exit, 830*time.Millisecond, output, clean)
	raw := mustRead(t, filepath.Join(dir, "t_9.jsonl"))
	for _, secret := range []string{"s3cr3tvalue", "Northwind#2026", "ghp_abcdefghijklmnopqrstuvwxyz0123"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("the secret %q reached the trace: %s", secret, raw)
		}
	}
	c := readLines(t, filepath.Join(dir, "t_9.jsonl"))[0]["command"].(map[string]any)
	if c["guard"] != "allowed" || c["exit"] != 0.0 || c["ms"] != 830.0 {
		t.Fatalf("command = %v", c)
	}
	if head, _ := c["output_head"].(string); len(head) != OutputHead || !strings.HasPrefix(head, "logged in") {
		t.Fatalf("output_head is %d bytes: %.80q", len(head), head)
	}
	if sha, _ := c["output_sha256"].(string); len(sha) != 64 {
		t.Fatalf("output_sha256 = %v", c["output_sha256"])
	}
	// A command that did not run has a null exit.
	w.CommandRun(taskCtx(w), "rm -rf build", GuardAsked, nil, 0, "", nil)
	if c := readLines(t, filepath.Join(dir, "t_9.jsonl"))[1]["command"].(map[string]any); c["exit"] != nil || c["guard"] != "asked" {
		t.Fatalf("an asked command's line = %v", c)
	}
}

func TestATraceStopsAtItsCapWithOneTruncatedLine(t *testing.T) {
	dir := t.TempDir()
	w := New(dir)
	w.max = 4096
	for i := 0; i < 50; i++ {
		w.CommandRun(taskCtx(w), "echo hello", GuardAllowed, nil, 0, strings.Repeat("y", 300), nil)
	}
	path := filepath.Join(dir, "t_9.jsonl")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > w.max {
		t.Fatalf("the trace grew to %d bytes past its cap %d", info.Size(), w.max)
	}
	lines := readLines(t, path)
	if lines[len(lines)-1]["kind"] != "truncated" {
		t.Fatalf("the last line is %v, want truncated", lines[len(lines)-1])
	}
	for _, l := range lines[:len(lines)-1] {
		if l["kind"] == "truncated" {
			t.Fatal("more than one truncated line")
		}
	}
	// A new writer (soul restarted) on the same full file adds nothing past the cap.
	again := New(dir)
	again.max = 4096
	again.CommandRun(taskCtx(again), "echo more", GuardAllowed, nil, 0, "", nil)
	if after, _ := os.Stat(path); after.Size() > w.max {
		t.Fatalf("a restarted writer grew the file to %d", after.Size())
	}
}

func TestTheTraceIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are POSIX")
	}
	dir := filepath.Join(t.TempDir(), "traces")
	w := New(dir)
	w.CommandRun(taskCtx(w), "ls", GuardAllowed, nil, 0, "", nil)
	info, err := os.Stat(filepath.Join(dir, "t_9.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("trace mode = %v, want 0600", info.Mode().Perm())
	}
}

// A folder the trace cannot be written in fails nothing: the call is answered.
func TestAnUnwritableFolderFailsNothing(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := New(filepath.Join(blocker, "traces")) // a folder under a file: never creatable
	res, err := Calls(answering{}).Chat(taskCtx(w), "m", nil, llm.Options{Purpose: llm.PurposeGeneral})
	if err != nil || res.Text != "ok" {
		t.Fatalf("the call failed with the trace: %v %+v", err, res)
	}
	w.CommandRun(taskCtx(w), "ls", GuardAllowed, nil, 0, "", nil)
}

// A job's own secret values, of no credential's shape, are withheld from the trace too (C14).
func TestAJobsSecretIsWithheldFromTheTrace(t *testing.T) {
	dir := t.TempDir()
	w := New(dir)
	ctx := withheld.With(taskCtx(w), []string{"plumtree-cabinet-41"})
	w.CommandRun(ctx, "gui type e3 'plumtree-cabinet-41'", GuardAllowed, nil, 0, "typed plumtree-cabinet-41", nil)
	if raw := mustRead(t, filepath.Join(dir, "t_9.jsonl")); strings.Contains(raw, "plumtree") {
		t.Fatalf("the trace holds the job's secret: %s", raw)
	}
}
