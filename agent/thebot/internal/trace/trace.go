// Package trace is soul's own record of a task: one decurix.soul-turn/v1 line per model call and per command the
// operator ran, appended to <WorkspaceRoot>/traces/<taskId>.jsonl (PLAN-P2 C4, §5.4). The control plane pulls the file
// with the run's bundle; soul never reads it back. For a local brain, which the control plane's proxy never sees, it
// is the only record of the model's calls.
//
// The trace is decoration: a write that fails is logged and swallowed, and never fails the call or the command.
// Everything written goes through soul's secrets filter (memory.RedactSecrets) first.
//
// Beside it, <taskId>.rules holds "sha256:<hex>" of the operator's rules text the task's latest job was given
// (Rules): the desktop's run manifest copies it as rules_sha (agent retention.py), so a run record names the rules
// it ran under. The desktop's retention prunes both files once the run is exported and a few days old.
package trace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"

	"thebot/internal/llm"
	"thebot/internal/memory"
	"thebot/internal/withheld"
)

// Schema names a trace line.
const Schema = "decurix.soul-turn/v1"

// MaxBytes bounds one task's trace file. Past it the file gets one last line, {"kind":"truncated"}, and no more.
const MaxBytes = 32 << 20

// OutputHead is how much of a command's output a line keeps, after redaction.
const OutputHead = 2 << 10

// Guard decisions on a command (soul's safety.AssessCommand and the screen's approval gate).
const (
	GuardAllowed = "allowed" // nothing in it needed a person's yes
	GuardAsked   = "asked"   // a person was asked: it ran on their yes, or is paused waiting for it
	GuardRefused = "refused" // held and not run, with nobody asked
)

// Writer appends trace lines, one file per task, under one directory.
type Writer struct {
	dir string
	max int64

	mu    sync.Mutex
	tasks map[string]*taskFile
}

// taskFile is what the writer knows of one task's file this process.
type taskFile struct {
	size    int64
	stopped bool // past max: the truncated line is written, nothing more is
	failed  bool // a write failed: logged once
}

// New is a writer under dir (created on the first line, 0700); files are 0600.
func New(dir string) *Writer {
	return &Writer{dir: dir, max: MaxBytes, tasks: map[string]*taskFile{}}
}

type writerKey struct{}

// WithWriter returns ctx carrying w: the model calls and commands made under it are traced.
func WithWriter(ctx context.Context, w *Writer) context.Context {
	if w == nil {
		return ctx
	}
	return context.WithValue(ctx, writerKey{}, w)
}

// From is the writer ctx carries, or nil (every Writer method is nil-safe).
func From(ctx context.Context) *Writer {
	if ctx == nil {
		return nil
	}
	w, _ := ctx.Value(writerKey{}).(*Writer)
	return w
}

// Usage is a call's tokens.
type Usage struct {
	In     int `json:"in"`
	Cached int `json:"cached"`
	Out    int `json:"out"`
}

// Command is what a command line records of one command.
type Command struct {
	Text         string `json:"text"`
	Guard        string `json:"guard"`
	Exit         *int   `json:"exit"`
	Ms           int64  `json:"ms"`
	OutputSHA256 string `json:"output_sha256"`
	OutputHead   string `json:"output_head"`
}

// Line is one decurix.soul-turn/v1 line.
type Line struct {
	Schema       string   `json:"schema"`
	At           string   `json:"at"`
	Task         string   `json:"task"`
	Job          string   `json:"job"`
	Turn         string   `json:"turn"`
	Kind         string   `json:"kind"` // call | command | truncated
	Purpose      string   `json:"purpose,omitempty"`
	Model        string   `json:"model,omitempty"`
	Provider     string   `json:"provider,omitempty"`
	PromptSHA256 string   `json:"prompt_sha256,omitempty"`
	Messages     int      `json:"messages,omitempty"`
	Usage        *Usage   `json:"usage,omitempty"`
	Ms           int64    `json:"ms,omitempty"`
	Failed       bool     `json:"failed,omitempty"`
	Command      *Command `json:"command"`
}

// truncatedLine is the last line of a trace that reached MaxBytes.
var truncatedLine = []byte(`{"schema":"` + Schema + `","kind":"truncated"}` + "\n")

// Call writes the line of one model call made under ctx: purpose, provider, model, the number of messages, the
// SHA-256 of the message list's JSON, usage and duration. Nothing of the messages themselves is written.
func (w *Writer) Call(ctx context.Context, purpose, provider, model string, messages []llm.Message, res llm.Result, d time.Duration, failed bool) {
	if w == nil {
		return
	}
	tags := llm.TagsFrom(ctx)
	if tags.Task == "" {
		return
	}
	body, _ := json.Marshal(messages) // a []llm.Message always marshals
	sum := sha256.Sum256(body)
	if purpose == "" {
		purpose = llm.PurposeUntagged
	}
	w.write(tags, Line{Kind: "call", Purpose: purpose, Model: model, Provider: provider,
		PromptSHA256: hex.EncodeToString(sum[:]), Messages: len(messages),
		Usage: &Usage{In: res.PromptTokens, Cached: res.CachedTokens, Out: res.ResponseTokens},
		Ms:    d.Milliseconds(), Failed: failed})
}

// CommandRun writes the line of one command run under ctx: its text and output through the secrets filter, the
// guard's decision, the exit code (nil when it did not run), duration, the redacted output's SHA-256 and its first
// OutputHead bytes. The job's secret values (withheld.From) are taken out first, then clean, the caller's own filter
// (the secrets the task gave; nil for none), then the shape filter.
func (w *Writer) CommandRun(ctx context.Context, text, guard string, exit *int, d time.Duration, output string, clean func(string) string) {
	if w == nil {
		return
	}
	tags := llm.TagsFrom(ctx)
	if tags.Task == "" {
		return
	}
	if clean == nil {
		clean = func(s string) string { return s }
	}
	secrets := withheld.From(ctx)
	own := clean
	clean = func(s string) string { return own(secrets.Text(s)) }
	redactedOut := memory.RedactSecrets(clean(output))
	sum := sha256.Sum256([]byte(redactedOut))
	w.write(tags, Line{Kind: "command", Command: &Command{
		Text: memory.RedactSecrets(clean(text)), Guard: guard, Exit: exit, Ms: d.Milliseconds(),
		OutputSHA256: hex.EncodeToString(sum[:]), OutputHead: head(redactedOut, OutputHead)}})
}

// head is s cut to at most n bytes, at a rune boundary.
func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// write appends one line for tags' task, under the mutex: every line whole, in order. A failure is logged once per
// task and swallowed.
func (w *Writer) write(tags llm.Tags, line Line) {
	if !llm.TaskIDShape.MatchString(tags.Task) {
		return
	}
	line.Schema, line.At = Schema, time.Now().UTC().Format(time.RFC3339Nano)
	line.Task, line.Job, line.Turn = tags.Task, tags.Job, tags.Turn
	b, err := json.Marshal(line)
	if err != nil {
		return
	}
	b = append(b, '\n')

	w.mu.Lock()
	defer w.mu.Unlock()
	tf := w.tasks[tags.Task]
	path := filepath.Join(w.dir, tags.Task+".jsonl")
	if tf == nil {
		tf = &taskFile{}
		if info, err := os.Stat(path); err == nil {
			tf.size = info.Size() // soul restarted: the file goes on where it was
			// A file that already ends with the truncated line is stopped for good: a restart never adds another.
			tf.stopped = endsTruncated(path, tf.size)
		}
		w.tasks[tags.Task] = tf
	}
	if tf.stopped {
		return
	}
	if tf.size+int64(len(b))+int64(len(truncatedLine)) > w.max {
		tf.stopped = true
		b = truncatedLine
	}
	if err := w.appendFile(path, b); err != nil {
		if !tf.failed {
			tf.failed = true
			log.Printf("[soul] trace: could not write %s: %v (the task goes on without it)", path, err)
		}
		return
	}
	tf.size += int64(len(b))
}

// endsTruncated reports whether the file at path, size bytes long, ends with the truncated line.
func endsTruncated(path string, size int64) bool {
	n := int64(len(truncatedLine))
	if size < n {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	tail := make([]byte, n)
	if _, err := f.ReadAt(tail, size-n); err != nil {
		return false
	}
	return string(tail) == string(truncatedLine)
}

// Rules notes the operator's rules text a job of ctx's task was given: "sha256:<hex>" of rules, in
// <dir>/<taskId>.rules (0600), replaced whole. Only the hash is written, never the text. Decoration: a failure is
// logged and swallowed.
func (w *Writer) Rules(ctx context.Context, rules string) {
	if w == nil {
		return
	}
	tags := llm.TagsFrom(ctx)
	if !llm.TaskIDShape.MatchString(tags.Task) {
		return
	}
	sum := sha256.Sum256([]byte(rules))
	body := []byte("sha256:" + hex.EncodeToString(sum[:]) + "\n")
	path := filepath.Join(w.dir, tags.Task+".rules")
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.replaceFile(path, body); err != nil {
		log.Printf("[soul] trace: could not note the rules of %s: %v (the task goes on without it)", tags.Task, err)
	}
}

// replaceFile writes b to path whole: a temporary file in the folder (0600), then renamed over it.
func (w *Writer) replaceFile(path string, b []byte) error {
	if err := os.MkdirAll(w.dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(w.dir, ".rules-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	cerr := f.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp, path)
	}
	if werr != nil {
		_ = os.Remove(tmp)
	}
	return werr
}

// appendFile appends b to path, creating the folder (0700) and the file (0600) when needed.
func (w *Writer) appendFile(path string, b []byte) error {
	if err := os.MkdirAll(w.dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(b)
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	if cerr != nil {
		return cerr
	}
	return nil
}

// Calls wraps a provider so each Chat call made under a context carrying a Writer (WithWriter) and a task
// (llm.WithTags) leaves one call line. Put it inside llm.Metered, which settles the call's purpose first.
func Calls(inner llm.Provider) llm.Provider { return &traced{inner: inner} }

type traced struct{ inner llm.Provider }

func (t *traced) Name() string                         { return t.inner.Name() }
func (t *traced) IsReachable(ctx context.Context) bool { return t.inner.IsReachable(ctx) }

// Chat delegates, then writes the call's line, failed or not.
func (t *traced) Chat(ctx context.Context, model string, messages []llm.Message, opts llm.Options) (llm.Result, error) {
	start := time.Now()
	res, err := t.inner.Chat(ctx, model, messages, opts)
	From(ctx).Call(ctx, opts.Purpose, t.inner.Name(), model, messages, res, time.Since(start), err != nil)
	return res, err
}

// errCannotEmbed is a wrapped provider that has no embeddings.
var errCannotEmbed = errors.New("cannot embed")

// Embed passes straight through, as Metered's does.
func (t *traced) Embed(ctx context.Context, model, text string) ([]float32, error) {
	e, ok := t.inner.(llm.Embedder)
	if !ok {
		return nil, fmt.Errorf("%s %w", t.inner.Name(), errCannotEmbed)
	}
	return e.Embed(ctx, model, text)
}
