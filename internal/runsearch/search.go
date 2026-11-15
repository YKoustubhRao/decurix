// search.go is the three shapes of a search over past runs (after hermes-agent's session search, in our own terms):
// discovery (words: the best-matching tasks, one per task however many of its texts match, a person's runs before a
// program's, only the top one opened), read (one task's texts in order) and scroll (a window around one text). Every
// shape answers with the stored text, each piece cut at MaxMessageChars and said so: never a summary.

package runsearch

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// Limits of the search shapes.
const (
	// MaxMessageChars is how much of one stored text any shape returns; the rest is said, never summarised.
	MaxMessageChars = 2000
	// DefaultLimit and MaxLimit bound the tasks a discovery returns.
	DefaultLimit = 5
	MaxLimit     = 20
	// scanLimit is how many matching texts are ranked before they are folded by task: well above the tasks a search
	// returns, so a person's run buried under a program's many matches survives the demotion.
	scanLimit = 300
	// MaxExclude bounds the task ids a search may leave out.
	MaxExclude = 20
	// topWindow is how many texts on each side of the top hit's match are opened.
	topWindow = 3
	// DefaultRead and MaxRead bound the texts a read returns (half from the start, half from the end).
	DefaultRead = 80
	MaxRead     = 200
	// DefaultWindow and MaxWindow bound a scroll on each side of its anchor.
	DefaultWindow = 5
	MaxWindow     = 20
	// maxWords bounds the words of one search.
	maxWords = 32
)

// ErrNoWords is a discovery with nothing to search for.
var ErrNoWords = errors.New("runsearch: give words to search for")

// ErrNotFound is a task the index does not hold, or does not hold for the worker asking.
var ErrNotFound = errors.New("runsearch: no such run")

// Query is a discovery search.
type Query struct {
	Words string
	// Worker, when set, holds the search to that worker's own tasks.
	Worker string
	// After and Before bound when the matching text was written (zero: no bound); Before is exclusive.
	After, Before time.Time
	// Exclude names tasks to leave out (ones already read), at most MaxExclude.
	Exclude []string
	Limit   int
}

// Message is one stored text of a task.
type Message struct {
	Seq         int       `json:"seq"`
	Kind        string    `json:"kind"`
	Actor       string    `json:"actor"`
	ExecutionID string    `json:"execution_id"`
	At          time.Time `json:"at"`
	Text        string    `json:"text"`
	// Cut is how many characters of the stored text are not in Text (0: it is whole).
	Cut int `json:"cut"`
}

// Hit is one task a discovery found.
type Hit struct {
	TaskID    string    `json:"task_id"`
	WorkerID  string    `json:"worker_id"`
	Origin    string    `json:"origin"`
	Automated bool      `json:"automated"`
	Status    string    `json:"status"`
	Outcome   string    `json:"outcome"`
	CreatedAt time.Time `json:"created_at"`
	// Seq and Kind are the best-matching text's, the anchor to scroll around; Snippet is its matching words.
	Seq     int    `json:"seq"`
	Kind    string `json:"kind"`
	Snippet string `json:"snippet"`
	// Messages is set for the top hit only: its request, the texts around the match, and its last report.
	Messages []Message `json:"messages,omitempty"`
}

// Result is what a discovery found.
type Result struct {
	Hits []Hit `json:"hits"`
	// Tasks is how many distinct tasks matched within the texts ranked.
	Tasks int    `json:"tasks"`
	Note  string `json:"note,omitempty"`
}

// Run is one task's texts, as a read or a scroll returns them.
type Run struct {
	TaskID    string    `json:"task_id"`
	WorkerID  string    `json:"worker_id"`
	Origin    string    `json:"origin"`
	Automated bool      `json:"automated"`
	Status    string    `json:"status"`
	Outcome   string    `json:"outcome"`
	CreatedAt time.Time `json:"created_at"`
	// Total is how many texts the task has; Messages are those returned, in order.
	Total    int       `json:"total"`
	Messages []Message `json:"messages"`
	Note     string    `json:"note,omitempty"`
}

// wordRe is a word of a search: letters and digits.
var wordRe = regexp.MustCompile(`[\p{L}\p{N}]+`)

// matchExpr is the FTS5 query for words: each word quoted (no FTS operator a caller types is ever read as one),
// joined with OR so a text matching more of them ranks higher.
func matchExpr(words string) string {
	found := wordRe.FindAllString(words, -1)
	if len(found) > maxWords {
		found = found[:maxWords]
	}
	parts := make([]string, 0, len(found))
	for _, w := range found {
		parts = append(parts, `"`+strings.ReplaceAll(w, `"`, `""`)+`"`)
	}
	return strings.Join(parts, " OR ")
}

// cut is text at most MaxMessageChars long, and how many characters were left out.
func cut(text string) (string, int) {
	if utf8.RuneCountInString(text) <= MaxMessageChars {
		return text, 0
	}
	r := []rune(text)
	return string(r[:MaxMessageChars]), len(r) - MaxMessageChars
}

// taskMeta is a row of run_search_tasks.
type taskMeta struct {
	worker, origin, status, outcome string
	automated                       bool
	created                         int64
}

// meta reads one indexed task's row; ErrNotFound when it is not indexed, or not worker's (when worker is set).
func (x *Index) meta(taskID, worker string) (taskMeta, error) {
	var m taskMeta
	var auto int
	err := x.db.QueryRow(`SELECT worker_id, origin, automated, status, outcome, created_ns FROM run_search_tasks WHERE task_id = ?`,
		taskID).Scan(&m.worker, &m.origin, &auto, &m.status, &m.outcome, &m.created)
	if err == nil && worker != "" && !strings.EqualFold(m.worker, worker) {
		err = ErrNotFound
	}
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	m.automated = auto == 1
	return m, err
}

// messages reads a task's texts with seq in [from, to], in order.
func (x *Index) messages(taskID string, from, to int) ([]Message, error) {
	return sqlitedb.QueryAll(x.db, func(s sqlitedb.Scanner) (Message, error) {
		var m Message
		var at int64
		var body string
		if err := s.Scan(&m.Seq, &m.Kind, &m.Actor, &m.ExecutionID, &at, &body); err != nil {
			return m, err
		}
		m.At = sqlitedb.FromNS(at)
		m.Text, m.Cut = cut(body)
		return m, nil
	}, `SELECT seq, kind, actor, execution_id, at_ns, body FROM run_search_docs WHERE task_id = ? AND seq BETWEEN ? AND ?
		ORDER BY seq`, taskID, from, to)
}

// count is how many texts a task has.
func (x *Index) count(taskID string) (int, error) {
	var n int
	err := x.db.QueryRow(`SELECT COUNT(*) FROM run_search_docs WHERE task_id = ?`, taskID).Scan(&n)
	return n, err
}

// match is one ranked matching text.
type match struct {
	task, kind, snippet string
	seq                 int
	automated           bool
}

// Search is a discovery: the tasks whose texts best match q.Words (BM25), each task once by its best-matching text,
// a person's runs before a program's (a program's are kept, ranked after), at most q.Limit. Only the top one is opened
// (its request, the texts around the match, its last report); the others carry one matching snippet.
func (x *Index) Search(q Query) (Result, error) {
	expr := matchExpr(q.Words)
	if expr == "" {
		return Result{}, ErrNoWords
	}
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	where := []string{"run_search_fts MATCH ?"}
	args := []any{expr}
	if q.Worker != "" {
		where = append(where, "d.worker_id = ?")
		args = append(args, q.Worker)
	}
	if !q.After.IsZero() {
		where = append(where, "d.at_ns >= ?")
		args = append(args, sqlitedb.ToNS(q.After))
	}
	if !q.Before.IsZero() {
		where = append(where, "d.at_ns < ?")
		args = append(args, sqlitedb.ToNS(q.Before))
	}
	excluded := 0
	for _, id := range q.Exclude {
		if id = strings.TrimSpace(id); id == "" {
			continue
		}
		if excluded == MaxExclude {
			break
		}
		where = append(where, "d.task_id != ?")
		args = append(args, id)
		excluded++
	}
	args = append(args, scanLimit)
	rows, err := sqlitedb.QueryAll(x.db, func(s sqlitedb.Scanner) (match, error) {
		var m match
		var auto int
		err := s.Scan(&m.task, &m.seq, &m.kind, &m.snippet, &auto)
		m.automated = auto == 1
		return m, err
	}, `SELECT d.task_id, d.seq, d.kind, snippet(run_search_fts, 0, '[[', ']]', '...', 16), t.automated
		FROM run_search_fts JOIN run_search_docs d ON d.id = run_search_fts.rowid
		JOIN run_search_tasks t ON t.task_id = d.task_id
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY bm25(run_search_fts), d.task_id, d.seq LIMIT ?`, args...)
	if err != nil {
		return Result{}, err
	}

	// One per task (its best text), a person's first, each class in rank order.
	seen := map[string]bool{}
	var people, programs []match
	for _, m := range rows {
		if seen[m.task] {
			continue
		}
		seen[m.task] = true
		if m.automated {
			programs = append(programs, m)
		} else {
			people = append(people, m)
		}
	}
	ordered := append(people, programs...)
	res := Result{Hits: []Hit{}, Tasks: len(ordered)}
	if len(ordered) > limit {
		ordered = ordered[:limit]
	}
	for i, m := range ordered {
		meta, err := x.meta(m.task, "")
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return Result{}, err
		}
		h := Hit{TaskID: m.task, WorkerID: meta.worker, Origin: meta.origin, Automated: meta.automated,
			Status: meta.status, Outcome: meta.outcome, CreatedAt: sqlitedb.FromNS(meta.created),
			Seq: m.seq, Kind: m.kind, Snippet: m.snippet}
		if i == 0 {
			if h.Messages, err = x.opened(m.task, m.seq); err != nil {
				return Result{}, err
			}
		}
		res.Hits = append(res.Hits, h)
	}
	if len(rows) == scanLimit {
		res.Note = fmt.Sprintf("only the %d best-matching texts were ranked; narrow the words or the time to see others", scanLimit)
	}
	return res, nil
}

// opened is the top hit's texts: the request, the texts within topWindow of the match, and the last report.
func (x *Index) opened(taskID string, seq int) ([]Message, error) {
	from := seq - topWindow
	if from < 0 {
		from = 0
	}
	window, err := x.messages(taskID, from, seq+topWindow)
	if err != nil {
		return nil, err
	}
	out := []Message{}
	if from > 0 {
		head, err := x.messages(taskID, 0, 0)
		if err != nil {
			return nil, err
		}
		out = append(out, head...)
	}
	out = append(out, window...)
	var lastSeq int
	err = x.db.QueryRow(`SELECT COALESCE(MAX(seq), -1) FROM run_search_docs WHERE task_id = ? AND kind = ?`, taskID, KindOutcome).Scan(&lastSeq)
	if err != nil {
		return nil, err
	}
	if lastSeq > seq+topWindow {
		last, err := x.messages(taskID, lastSeq, lastSeq)
		if err != nil {
			return nil, err
		}
		out = append(out, last...)
	}
	return out, nil
}

// runOf is a Run's header for an indexed task.
func runOf(taskID string, m taskMeta, total int) Run {
	return Run{TaskID: taskID, WorkerID: m.worker, Origin: m.origin, Automated: m.automated, Status: m.status,
		Outcome: m.outcome, CreatedAt: sqlitedb.FromNS(m.created), Total: total, Messages: []Message{}}
}

// Read is one task's texts in order (worker, when set, must be the task's). With more than limit texts, the first
// and last halves are returned and the middle is said, to be scrolled to.
func (x *Index) Read(taskID, worker string, limit int) (Run, error) {
	m, err := x.meta(taskID, worker)
	if err != nil {
		return Run{}, err
	}
	if limit <= 0 {
		limit = DefaultRead
	}
	if limit > MaxRead {
		limit = MaxRead
	}
	total, err := x.count(taskID)
	if err != nil {
		return Run{}, err
	}
	run := runOf(taskID, m, total)
	if total <= limit {
		run.Messages, err = x.messages(taskID, 0, total-1)
		return run, err
	}
	head := limit / 2
	tail := limit - head
	first, err := x.messages(taskID, 0, head-1)
	if err != nil {
		return Run{}, err
	}
	last, err := x.messages(taskID, total-tail, total-1)
	if err != nil {
		return Run{}, err
	}
	run.Messages = append(first, last...)
	run.Note = fmt.Sprintf("texts %d to %d of %d are not shown here: scroll around one of them to read them",
		head, total-tail-1, total)
	return run, nil
}

// Scroll is the texts of one task within window of the text numbered around (worker, when set, must be the task's).
func (x *Index) Scroll(taskID, worker string, around, window int) (Run, error) {
	m, err := x.meta(taskID, worker)
	if err != nil {
		return Run{}, err
	}
	if window <= 0 {
		window = DefaultWindow
	}
	if window > MaxWindow {
		window = MaxWindow
	}
	total, err := x.count(taskID)
	if err != nil {
		return Run{}, err
	}
	run := runOf(taskID, m, total)
	from := around - window
	if from < 0 {
		from = 0
	}
	run.Messages, err = x.messages(taskID, from, around+window)
	return run, err
}

// relativeRe is a relative time bound: 24h, 7d, 2w.
var relativeRe = regexp.MustCompile(`^(\d{1,5})\s*([hdw])$`)

// ParseBound reads a time bound: RFC 3339, a date (YYYY-MM-DD, midnight UTC), or a relative one ("7d", "24h", "2w":
// now less that much). "" is no bound (the zero time).
func ParseBound(s string, now time.Time) (time.Time, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return time.Time{}, nil
	}
	if m := relativeRe.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		unit := map[string]time.Duration{"h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour}[m[2]]
		return now.Add(-time.Duration(n) * unit), nil
	}
	if t, err := time.Parse(time.RFC3339, strings.ToUpper(s)); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("runsearch: a time bound is a date (2026-07-01), a time (RFC 3339) or a span back from now (24h, 7d, 2w)")
}
