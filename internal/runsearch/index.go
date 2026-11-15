// Package runsearch is the search over past runs: an SQLite FTS5 index of what the task records already keep, in the
// form they keep it (withheld of secrets at the source): each task's request, the steps its attempts reported, the
// questions and approvals it put to a person with the answers they gave, its final reports, and a person's verdict.
// The chat thread is a view of these same records (it keeps only the newest entries), so it is not indexed again.
//
// The index is derived, never a label: a task's rows are rewritten whenever its record changes, and nothing here is
// the only copy of anything (plan rule 4 holds for the task records it reads). Every search answers with stored text
// only, cut to a length and said so, never a model's summary of it.
//
// It reads internal/tasks (a TaskSource, and the tasks table's change times for its sweep) and imports nothing of
// httpapi.
package runsearch

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
	"github.com/YKoustubhRao/decurix/internal/tasks"
	"github.com/YKoustubhRao/decurix/internal/withhold"
)

// schema is the runsearch component's migration steps. Append only: an applied step is never edited.
var schema = []string{
	// One row per piece of stored text of a task, in the order a person reads it (seq). Rewritten whole when the
	// task's record changes.
	`CREATE TABLE run_search_docs (
		id           INTEGER PRIMARY KEY,
		task_id      TEXT NOT NULL,
		execution_id TEXT NOT NULL DEFAULT '',
		worker_id    TEXT NOT NULL,
		seq          INTEGER NOT NULL,
		kind         TEXT NOT NULL,
		actor        TEXT NOT NULL DEFAULT '',
		at_ns        INTEGER NOT NULL,
		body         TEXT NOT NULL
	)`,
	`CREATE INDEX run_search_docs_task ON run_search_docs (task_id, seq)`,
	// One row per indexed task: who ran it, what started it, how it ended, and the hash of what was indexed.
	`CREATE TABLE run_search_tasks (
		task_id    TEXT PRIMARY KEY,
		worker_id  TEXT NOT NULL,
		origin     TEXT NOT NULL DEFAULT '',
		automated  INTEGER NOT NULL DEFAULT 0,
		status     TEXT NOT NULL DEFAULT '',
		outcome    TEXT NOT NULL DEFAULT '',
		created_ns INTEGER NOT NULL,
		indexed_ns INTEGER NOT NULL,
		docs_sha   TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX run_search_tasks_worker ON run_search_tasks (worker_id, created_ns)`,
	// The full-text index over the docs' bodies, kept in step by the triggers below (an external-content table: the
	// text is stored once, in run_search_docs).
	`CREATE VIRTUAL TABLE run_search_fts USING fts5(body, content='run_search_docs', content_rowid='id',
		tokenize='unicode61 remove_diacritics 2')`,
	`CREATE TRIGGER run_search_docs_ai AFTER INSERT ON run_search_docs BEGIN
		INSERT INTO run_search_fts(rowid, body) VALUES (new.id, new.body);
	END`,
	`CREATE TRIGGER run_search_docs_ad AFTER DELETE ON run_search_docs BEGIN
		INSERT INTO run_search_fts(run_search_fts, rowid, body) VALUES ('delete', old.id, old.body);
	END`,
	`CREATE TRIGGER run_search_docs_au AFTER UPDATE OF body ON run_search_docs BEGIN
		INSERT INTO run_search_fts(run_search_fts, rowid, body) VALUES ('delete', old.id, old.body);
		INSERT INTO run_search_fts(rowid, body) VALUES (new.id, new.body);
	END`,
	// How far the sweep has read the tasks table: the change time and id of the last task it indexed.
	`CREATE TABLE run_search_state (
		name TEXT PRIMARY KEY,
		ns   INTEGER NOT NULL,
		id   TEXT NOT NULL DEFAULT ''
	)`,
}

// Doc kinds: what a piece of indexed text is.
const (
	KindRequest  = "request"
	KindStep     = "step"
	KindQuestion = "question"
	KindApproval = "approval"
	KindAnswer   = "answer"
	KindOutcome  = "outcome"
	KindVerdict  = "verdict"
)

// maxDocChars bounds one indexed text; a longer one is kept to this length and says it was cut.
const maxDocChars = 32000

// lateWait is how long after a task ends it is indexed: a desktop's last steps may arrive up to 10 s after its outcome
// (tasks.lateWindow).
const lateWait = 15 * time.Second

// sweepBatch is how many changed tasks one pass of the sweep reads.
const sweepBatch = 200

// TaskSource is what the index reads a task from: *tasks.Store.
type TaskSource interface {
	Get(taskID string) (tasks.Detail, error)
}

// Index is the run search index over one database.
type Index struct {
	db    *sql.DB
	tasks TaskSource

	// mu serialises a task's read-then-rewrite and the sweep's watermark, so an older read never overwrites a newer.
	mu sync.Mutex

	// LateWait is how long TaskEnded waits before indexing (default lateWait); a test sets it short.
	LateWait time.Duration
	now      func() time.Time
}

// New migrates the runsearch schema on db and returns the index, reading tasks from src.
func New(db *sql.DB, src TaskSource) (*Index, error) {
	if err := sqlitedb.Migrate(db, "runsearch", schema); err != nil {
		return nil, err
	}
	return &Index{db: db, tasks: src, now: func() time.Time { return time.Now().UTC() }}, nil
}

// doc is one piece of a task's stored text, before it is written.
type doc struct {
	execID, kind, actor string
	at                  time.Time
	body                string
}

// Automated reports whether an origin kind is a program's (a schedule, a mail trigger, the queue, a DAG, a handoff, a
// replayed recording, a practice or eval run) rather than a person's: such runs rank below a person's.
func Automated(origin string) bool {
	switch origin {
	case tasks.OriginSchedule, tasks.OriginMail, tasks.OriginQueue, tasks.OriginDAG, tasks.OriginHandoff,
		tasks.OriginRecording, tasks.OriginPractice, tasks.OriginEval:
		return true
	}
	return false
}

// clean is a body as it is indexed: the records' own withheld form, with the fail-safe backstops of the control
// plane applied once more (withhold.Text, then the values held for every writer), cut to maxDocChars.
func clean(text string) string {
	text = strings.TrimSpace(withhold.Known(withhold.Text(text)))
	if utf8.RuneCountInString(text) > maxDocChars {
		r := []rune(text)
		text = string(r[:maxDocChars]) + fmt.Sprintf(" [cut here: %d more characters are in the task record]", len(r)-maxDocChars)
	}
	return text
}

// docsOf is a task's stored text in reading order: the request, then each attempt's steps and questions (with their
// answers) in time order and its final report, then a person's verdict.
func docsOf(d tasks.Detail) []doc {
	var out []doc
	add := func(x doc) {
		if x.body = clean(x.body); x.body != "" {
			out = append(out, x)
		}
	}
	firstActor := ""
	if len(d.Executions) > 0 {
		firstActor = d.Executions[0].Origin.ActorID
	}
	add(doc{kind: KindRequest, actor: firstActor, at: d.CreatedAt, body: d.Request})
	for _, e := range d.Executions {
		for _, ev := range e.Timeline() {
			switch {
			case ev.Step != nil:
				add(doc{execID: e.ID, kind: KindStep, actor: "worker", at: ev.At, body: ev.Step.Text})
			case ev.Intervention != nil:
				iv := ev.Intervention
				kind := KindQuestion
				if iv.Kind == tasks.InterventionApproval {
					kind = KindApproval
				}
				add(doc{execID: e.ID, kind: kind, actor: "worker", at: iv.RequestedAt, body: iv.Request})
				if !iv.RepliedAt.IsZero() {
					reply := iv.Reply
					if iv.Secret {
						reply = withhold.Secret
					}
					add(doc{execID: e.ID, kind: KindAnswer, actor: iv.ReplierID, at: iv.RepliedAt, body: reply})
				}
			}
		}
		if e.FinalReply != "" {
			at := e.EndedAt
			if at.IsZero() {
				at = e.StartedAt
			}
			add(doc{execID: e.ID, kind: KindOutcome, actor: "worker", at: at, body: e.FinalReply})
		}
	}
	if d.Verdict != "" {
		body := "A person marked the result " + d.Verdict
		if strings.TrimSpace(d.VerdictNote) != "" {
			body += ": " + d.VerdictNote
		}
		add(doc{kind: KindVerdict, at: d.VerdictAt, body: body})
	}
	return out
}

// docsSHA is the hash of what a task's docs would index, to skip a rewrite that changes nothing.
func docsSHA(origin, status, outcome string, docs []doc) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00", origin, status, outcome)
	for _, d := range docs {
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%d\x00%s\x01", d.execID, d.kind, d.actor, d.at.UnixNano(), d.body)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// IndexTask (re)writes one task's rows from its record. A task the records no longer hold is taken out of the index.
func (x *Index) IndexTask(taskID string) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.indexTask(taskID)
}

// indexTask is IndexTask with x.mu held.
func (x *Index) indexTask(taskID string) error {
	d, err := x.tasks.Get(taskID)
	if errors.Is(err, tasks.ErrNotFound) {
		return x.forget(taskID)
	}
	if err != nil {
		return err
	}
	origin := ""
	if len(d.Executions) > 0 {
		origin = d.Executions[0].Origin.Kind
	}
	automated := Automated(origin) || strings.HasPrefix(d.WorkerID, "practice-") || strings.HasPrefix(d.WorkerID, "readiness-")
	outcome := tasks.DisplayOutcome(d.Status, d.Checked).State
	docs := docsOf(d)
	sha := docsSHA(origin, d.Status, outcome, docs)

	var old string
	err = x.db.QueryRow(`SELECT docs_sha FROM run_search_tasks WHERE task_id = ?`, taskID).Scan(&old)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil && old == sha {
		return nil
	}

	tx, err := x.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM run_search_docs WHERE task_id = ?`, taskID); err != nil {
		return err
	}
	for i, dc := range docs {
		if _, err := tx.Exec(`INSERT INTO run_search_docs (task_id, execution_id, worker_id, seq, kind, actor, at_ns, body)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, taskID, dc.execID, d.WorkerID, i, dc.kind, dc.actor, sqlitedb.ToNS(dc.at), dc.body); err != nil {
			return err
		}
	}
	auto := 0
	if automated {
		auto = 1
	}
	if _, err := tx.Exec(`INSERT INTO run_search_tasks (task_id, worker_id, origin, automated, status, outcome, created_ns, indexed_ns, docs_sha)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(task_id) DO UPDATE SET worker_id = excluded.worker_id, origin = excluded.origin,
			automated = excluded.automated, status = excluded.status, outcome = excluded.outcome,
			created_ns = excluded.created_ns, indexed_ns = excluded.indexed_ns, docs_sha = excluded.docs_sha`,
		taskID, d.WorkerID, origin, auto, d.Status, outcome, sqlitedb.ToNS(d.CreatedAt), sqlitedb.ToNS(x.now()), sha); err != nil {
		return err
	}
	return tx.Commit()
}

// forget takes a task out of the index (its record is gone: a deleted worker's tasks are kept, so this is rare).
func (x *Index) forget(taskID string) error {
	tx, err := x.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM run_search_docs WHERE task_id = ?`, taskID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM run_search_tasks WHERE task_id = ?`, taskID); err != nil {
		return err
	}
	return tx.Commit()
}

// TaskEnded indexes a task once its last late steps have arrived (LateWait after it closed). It never blocks: it is
// called from the task store's close hook (tasks.Store.OnTaskEnded).
func (x *Index) TaskEnded(_, taskID string) {
	if taskID == "" {
		return
	}
	wait := x.LateWait
	if wait <= 0 {
		wait = lateWait
	}
	time.AfterFunc(wait, func() {
		if err := x.IndexTask(taskID); err != nil {
			log.Printf("runsearch: indexing task %s: %v", taskID, err)
		}
	})
}

// changed is one task the sweep found changed: its id and its change time.
type changed struct {
	id string
	ns int64
}

// Sweep indexes up to limit tasks whose record changed since the last sweep (their status or a person's verdict),
// oldest change first, and moves the watermark past them. It returns how many it read. From an empty index the sweep
// is the backfill of every task already on record.
func (x *Index) Sweep(limit int) (int, error) {
	if limit <= 0 {
		limit = sweepBatch
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	var wmNS int64
	var wmID string
	err := x.db.QueryRow(`SELECT ns, id FROM run_search_state WHERE name = 'sweep'`).Scan(&wmNS, &wmID)
	if err != nil && err != sql.ErrNoRows {
		return 0, err
	}
	// The tasks table is internal/tasks's; it is only read here, for the change times its own writes keep.
	rows, err := sqlitedb.QueryAll(x.db, func(s sqlitedb.Scanner) (changed, error) {
		var c changed
		return c, s.Scan(&c.id, &c.ns)
	}, `SELECT id, MAX(updated_ns, verdict_ns) AS ch FROM tasks
		WHERE MAX(updated_ns, verdict_ns) > ? OR (MAX(updated_ns, verdict_ns) = ? AND id > ?)
		ORDER BY ch, id LIMIT ?`, wmNS, wmNS, wmID, limit)
	if err != nil {
		return 0, err
	}
	for _, c := range rows {
		if err := x.indexTask(c.id); err != nil {
			return 0, fmt.Errorf("task %s: %w", c.id, err)
		}
		if _, err := x.db.Exec(`INSERT INTO run_search_state (name, ns, id) VALUES ('sweep', ?, ?)
			ON CONFLICT(name) DO UPDATE SET ns = excluded.ns, id = excluded.id`, c.ns, c.id); err != nil {
			return 0, err
		}
	}
	return len(rows), nil
}

// Run backfills the index, then sweeps for changed tasks every interval until ctx ends.
func (x *Index) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	sweepAll := func() {
		for ctx.Err() == nil {
			n, err := x.Sweep(sweepBatch)
			if err != nil {
				log.Printf("runsearch: sweep: %v", err)
				return
			}
			if n < sweepBatch {
				return
			}
		}
	}
	sweepAll()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweepAll()
		}
	}
}

// PurgeValue overwrites every indexed copy of a leaked secret (in any of forms) with mark, for internal/purge, and
// returns how many rows held it. The full-text index follows through its update trigger, which takes the old terms
// out.
func (x *Index) PurgeValue(forms []string, mark string) (int64, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	n, err := sqlitedb.ReplaceInColumns(x.db, "run_search_docs", []string{"body"}, forms, mark)
	if err != nil {
		return n, err
	}
	// The hashes no longer describe the rows: the next change of each task rewrites it from its (purged) record.
	_, err = x.db.Exec(`UPDATE run_search_tasks SET docs_sha = ''`)
	return n, err
}
