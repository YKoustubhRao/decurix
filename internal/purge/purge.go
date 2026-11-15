// Package purge removes a leaked secret, by value, from every store the control plane never deletes from: the blob
// store (model-call bodies, run-bundle files, eval suites), the llm_calls rows, the judgements, the task records, the
// chat and the concierge's messages, the query notes, and the run-bundle manifests. Plan rule 4 keeps everything that
// labels a run; a secret is not a label, so a person may take it out (owner decision, docs/follow-ups.md, Phase 2).
//
// Nothing is deleted. Each stored copy of the value is overwritten by a mark in place, and every row, id and blob
// name stays, so the records still join: a row still names its blobs, a blob keeps the hash it was stored under
// (blobstore/purge.go keeps the hash of its new bytes beside it), a prompt prefix still matches. The purge is itself
// a labelled act (plan rule 5): a judgement of kind "purge" with the person, the reason they typed and the counts per
// store, never the value. The value is never logged, stored or echoed; a value shorter than MinRunes is refused, as
// too likely to match ordinary text.
//
// Bounded work: SQLite scans its own tables (one UPDATE per table), the tables a purge must also read are paged, and
// each blob is streamed, never held whole. One purge runs at a time.
//
// Imports the stores it purges and sqlitedb; nothing from httpapi. Served by httpapi/privacy_purge.go (primary
// listener only, humansOnly) and `decurix --purge-secret` (cmd/decurix/purge.go, which calls that route).
package purge

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/YKoustubhRao/decurix/internal/blobstore"
	"github.com/YKoustubhRao/decurix/internal/concierge"
	"github.com/YKoustubhRao/decurix/internal/conversation"
	"github.com/YKoustubhRao/decurix/internal/llmrec"
	"github.com/YKoustubhRao/decurix/internal/querynotes"
	"github.com/YKoustubhRao/decurix/internal/runs"
	"github.com/YKoustubhRao/decurix/internal/runsearch"
	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// Mark is what every stored copy of a purged value becomes (in a blob, Mark padded or cut to the value's length).
const Mark = "[purged]"

// MinRunes is the shortest value a purge accepts.
const MinRunes = 6

// Refusals: nothing was changed.
var (
	ErrTooShort = errors.New("the value is shorter than 6 characters: too likely to match ordinary text, so nothing was purged")
	ErrNoReason = errors.New("say why it is purged: the reason is kept with who purged it (never the value)")
	ErrNoPerson = errors.New("only a person can purge a secret")
	ErrBusy     = errors.New("another purge is running; try again when it has finished")
	ErrNoRecord = errors.New("the task records are not open, so the purge could not be recorded; nothing was purged")
	ErrIsMark   = errors.New("the value is part of the purge mark itself; nothing was purged")
)

// running lets one purge run at a time.
var running sync.Mutex

// Stores are what a purge rewrites. A nil store is skipped and said so in Result.Problems; Tasks is required (the
// purge is recorded there).
type Stores struct {
	DB        *sql.DB
	Blobs     *blobstore.Store
	Calls     *llmrec.Store
	Tasks     *tasks.Store
	Chat      *conversation.Store
	Concierge *concierge.Store
	Notes     *querynotes.Store
	Runs      *runs.Store
	// Search is the index of past runs' text (internal/runsearch), which holds copies of the task records' text.
	Search *runsearch.Index
}

// Counts is how many stored things held the value, per store.
type Counts struct {
	// Blobs is how many blobs were rewritten; BlobsFailed how many held it, or could not be read, and were left.
	Blobs       int64 `json:"blobs"`
	BlobsFailed int64 `json:"blobs_failed"`
	// LLMCalls is the calls whose row was rewritten or whose messages, answer or tools were a purged blob.
	LLMCalls    int64 `json:"llm_calls"`
	Judgements  int64 `json:"judgements"`
	TaskRecords int64 `json:"task_records"`
	// Chat is chat entries and steps, and the concierge's messages and sessions.
	Chat       int64 `json:"chat"`
	QueryNotes int64 `json:"query_notes"`
	// RunBundles is the bundles whose manifest was rewritten or which list a purged blob.
	RunBundles int64 `json:"run_bundles"`
	// RunSearch is the texts of the run search index that held it.
	RunSearch int64 `json:"run_search"`
}

// Total is every count but the failures.
func (c Counts) Total() int64 {
	return c.Blobs + c.LLMCalls + c.Judgements + c.TaskRecords + c.Chat + c.QueryNotes + c.RunBundles + c.RunSearch
}

// Result is what a purge did. It never holds the value.
type Result struct {
	Counts Counts `json:"counts"`
	// Judgement is the id of the judgement that records the purge.
	Judgement string `json:"judgement"`
	// Compacted is whether the database was rebuilt afterwards, so no free page keeps an old copy.
	Compacted bool `json:"compacted"`
	// Problems names each store that could not be purged, in words that never hold the value.
	Problems []string `json:"problems"`
}

// Forms is every form value may be stored in, longest first: as written, each JSON-string escaping
// (llmrec.SecretForms, the forms the recorder withholds), and base64 of it alone (standard and URL alphabets, padded
// and not).
func Forms(value string) []string {
	forms := llmrec.SecretForms(value)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding,
		base64.RawURLEncoding} {
		forms = append(forms, enc.EncodeToString([]byte(value)))
	}
	seen := map[string]bool{}
	out := forms[:0]
	for _, f := range forms {
		if f != "" && !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// Check is the refusal a purge of value would give before touching anything (nil when it would run).
func Check(value string) error {
	if utf8.RuneCountInString(strings.TrimSpace(value)) < MinRunes {
		return ErrTooShort
	}
	if strings.Contains(Mark, value) {
		return ErrIsMark
	}
	return nil
}

// Purge overwrites every stored copy of value with Mark, in every store, and records a judgement of actor's purge with
// reason and the counts (never the value). A refusal (Check, no reason, not a person, another purge running, no task
// records) changes nothing. Once it runs, a store that fails is named in Result.Problems and the others go on; the
// error is then only a failure to record the purge.
func (st Stores) Purge(ctx context.Context, value, reason string, actor tasks.Actor, door string) (Result, error) {
	if err := Check(value); err != nil {
		return Result{}, err
	}
	forms := Forms(value)
	reason, _ = sqlitedb.PurgeText(strings.TrimSpace(reason), forms, Mark)
	if reason == "" {
		return Result{}, ErrNoReason
	}
	if actor.Kind != tasks.ActorPerson || strings.TrimSpace(actor.ID) == "" {
		return Result{}, ErrNoPerson
	}
	if st.Tasks == nil {
		return Result{}, ErrNoRecord
	}
	if !running.TryLock() {
		return Result{}, ErrBusy
	}
	defer running.Unlock()

	res := Result{Judgement: tasks.NewJudgementID(), Problems: []string{}}
	problem := func(store string) {
		res.Problems = append(res.Problems, store+": could not be purged; run the purge again")
	}

	purged := map[string]bool{}
	if st.Blobs == nil {
		res.Problems = append(res.Problems, "blobs: not open")
	} else {
		failed, err := st.Blobs.PurgeForms(forms, Mark, res.Judgement, func(sha string) { purged[sha] = true })
		res.Counts.Blobs, res.Counts.BlobsFailed = int64(len(purged)), int64(failed)
		if err != nil {
			problem("blobs")
		} else if failed > 0 {
			res.Problems = append(res.Problems, "blobs: some could not be read or rewritten (in use?); run the purge again")
		}
	}
	if st.Calls != nil {
		n, err := st.Calls.PurgeValue(forms, Mark, purged)
		res.Counts.LLMCalls = n
		if err != nil {
			problem("llm_calls")
		}
	}
	if st.Runs != nil {
		n, err := st.Runs.PurgeValue(forms, Mark, purged)
		res.Counts.RunBundles = n
		if err != nil {
			problem("run bundles")
		}
	}
	records, judgements, err := st.Tasks.PurgeValue(forms, Mark)
	res.Counts.TaskRecords, res.Counts.Judgements = records, judgements
	if err != nil {
		problem("task records")
	}
	if st.Chat != nil {
		n, err := st.Chat.PurgeValue(forms, Mark)
		res.Counts.Chat += n
		if err != nil {
			problem("chat")
		}
	}
	if st.Concierge != nil {
		n, err := st.Concierge.PurgeValue(forms, Mark)
		res.Counts.Chat += n
		if err != nil {
			problem("concierge")
		}
	}
	if st.Notes != nil {
		n, err := st.Notes.PurgeValue(forms, Mark)
		res.Counts.QueryNotes = n
		if err != nil {
			problem("query notes")
		}
	}
	if st.Search != nil {
		n, err := st.Search.PurgeValue(forms, Mark)
		res.Counts.RunSearch = n
		if err != nil {
			problem("run search index")
		}
	}
	if st.DB != nil && res.Counts.Total() > 0 {
		res.Compacted = compact(ctx, st.DB)
	}

	payload, _ := json.Marshal(map[string]any{"counts": res.Counts, "compacted": res.Compacted, "problems": res.Problems})
	if _, err := st.Tasks.AddJudgement(tasks.Judgement{
		ID: res.Judgement, At: time.Now().UTC(), Actor: actor, Door: door, Kind: tasks.JudgementPurge,
		Text: reason, Payload: payload,
	}); err != nil {
		res.Problems = append(res.Problems, "the purge could not be recorded")
		return res, errors.New("purge: the purge ran but its judgement could not be recorded")
	}
	return res, nil
}

// compact rebuilds the database from its live rows and empties the write-ahead log, so no free page or old frame
// keeps a copy the purge overwrote. Best effort: false when it could not (a busy database); the rows are purged
// either way.
func compact(ctx context.Context, db *sql.DB) bool {
	conn, err := db.Conn(ctx)
	if err != nil {
		return false
	}
	defer conn.Close()
	// The rebuild's temporary copy goes to disk, not memory (sqlitedb.Open sets temp_store MEMORY).
	_, _ = conn.ExecContext(ctx, `PRAGMA temp_store = FILE`)
	defer func() { _, _ = conn.ExecContext(context.Background(), `PRAGMA temp_store = MEMORY`) }()
	if _, err := conn.ExecContext(ctx, `VACUUM`); err != nil {
		return false
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return false
	}
	return true
}
