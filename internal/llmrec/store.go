package llmrec

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/YKoustubhRao/decurix/internal/blobstore"
	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
	"github.com/YKoustubhRao/decurix/internal/withhold"
)

var schema = []string{
	`CREATE TABLE llm_calls (
		id TEXT PRIMARY KEY, at_ns INTEGER NOT NULL, ms INTEGER NOT NULL,
		provider TEXT NOT NULL, deployment TEXT NOT NULL DEFAULT '', status INTEGER NOT NULL,
		desktop_id TEXT NOT NULL DEFAULT '', task_id TEXT NOT NULL DEFAULT '', turn TEXT NOT NULL DEFAULT '',
		purpose TEXT NOT NULL, caller TEXT NOT NULL DEFAULT '',
		messages TEXT NOT NULL,
		prefix_sha TEXT NOT NULL,
		prompt_sha TEXT NOT NULL,
		params TEXT NOT NULL DEFAULT '{}',
		response_sha TEXT NOT NULL DEFAULT '',
		input_tokens INTEGER, cached_input_tokens INTEGER, output_tokens INTEGER,
		problems TEXT NOT NULL DEFAULT '[]'
	)`,
	`CREATE INDEX llm_calls_task ON llm_calls (task_id, at_ns)`,
	`CREATE INDEX llm_calls_desktop ON llm_calls (desktop_id, at_ns)`,
}

// Store keeps the call rows in SQLite and their bodies in a blob store. It never updates or deletes a row.
type Store struct {
	db       *sql.DB
	blobs    *blobstore.Store
	failures atomic.Int64
	now      func() time.Time
}

// NewStore opens the call log on db, with bodies in blobs.
func NewStore(db *sql.DB, blobs *blobstore.Store) (*Store, error) {
	if db == nil || blobs == nil {
		return nil, errors.New("llmrec: a database and a blob store are needed")
	}
	if err := sqlitedb.Migrate(db, "llmrec", schema); err != nil {
		return nil, err
	}
	return &Store{db: db, blobs: blobs, now: time.Now}, nil
}

// Blobs is the store the bodies are kept in.
func (s *Store) Blobs() *blobstore.Store { return s.blobs }

// Failures is how many calls could not be recorded since the store opened.
func (s *Store) Failures() int64 { return s.failures.Load() }

// Record stores one call: its messages and answer withheld and content-addressed, and its row. It never returns an
// error: a failure is logged and counted (Failures), since the call it records has been answered either way.
func (s *Store) Record(_ context.Context, c Call) {
	if err := s.record(c); err != nil {
		s.failures.Add(1)
		log.Printf("llmrec: a %s call of %q could not be recorded: %v", c.Provider, c.Desktop, err)
	}
}

// record is Record, with its error.
func (s *Store) record(c Call) error {
	if s == nil || s.db == nil {
		return errors.New("no store")
	}
	problems := append([]string(nil), c.Problems...)
	w := withholder{secrets: longestFirst(c.Secrets), blobs: s.blobs}

	shas := []string{}
	params := "{}"
	var req map[string]any
	if err := decode(c.Request, &req); err != nil || req == nil {
		problems = append(problems, "body not stored: the request is not a JSON object")
	} else {
		msgs, _ := req["messages"].([]any)
		for _, m := range msgs {
			canon, err := json.Marshal(w.value(m))
			if err != nil {
				return err
			}
			sha, err := s.blobs.PutBytes(canon)
			if err != nil {
				return err
			}
			shas = append(shas, sha)
		}
		delete(req, "messages")
		// The tool definitions are the same on every call of a turn and large: kept once, as a blob.
		if tools, ok := req["tools"]; ok {
			canon, err := json.Marshal(w.value(tools))
			if err != nil {
				return err
			}
			sha, err := s.blobs.PutBytes(canon)
			if err != nil {
				return err
			}
			req["tools"] = map[string]any{"type": "blob_ref", "sha256": sha}
		}
		b, err := json.Marshal(w.value(req))
		if err != nil {
			return err
		}
		params = string(b)
	}
	problems = append(problems, w.problems...)

	responseSHA := ""
	if len(c.Response) > 0 {
		var answer any
		var body []byte
		if decode(c.Response, &answer) == nil {
			b, err := json.Marshal(w.value(answer))
			if err != nil {
				return err
			}
			body = b
		} else {
			body = []byte(w.text(string(c.Response)))
		}
		sha, err := s.blobs.PutBytes(body)
		if err != nil {
			return err
		}
		responseSHA = sha
	}

	at := c.At
	if at.IsZero() {
		at = s.now().Add(-c.Latency)
	}
	messages, _ := json.Marshal(shas)
	probs, _ := json.Marshal(dedupe(problems))
	var in, cached, out any
	if c.Usage.Tokens() > 0 {
		in, cached, out = c.Usage.Input, c.Usage.Cached, c.Usage.Output
	}

	_, err := s.db.Exec(`INSERT INTO llm_calls (id, at_ns, ms, provider, deployment, status, desktop_id, task_id, turn,
		purpose, caller, messages, prefix_sha, prompt_sha, params, response_sha, input_tokens, cached_input_tokens,
		output_tokens, problems) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sqlitedb.NewID("c", 8), sqlitedb.ToNS(at), c.Latency.Milliseconds(), c.Provider, c.Deployment, c.Status,
		c.Desktop, c.Task, c.Turn, orUntagged(c.Purpose), c.Caller, string(messages), PrefixSHA(shas), PromptSHA(shas),
		params, responseSHA, in, cached, out, string(probs))
	return err
}

// decode reads JSON keeping numbers as written (a seed or an id past 2^53 is not rounded).
func decode(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.More() {
		return errors.New("more than one JSON value")
	}
	return nil
}

// PromptSHA is the hash of a call's whole message list, as stored: the SHA-256 of its message hashes, one per line.
func PromptSHA(messageSHAs []string) string {
	return blobstore.SHA256([]byte(strings.Join(messageSHAs, "\n")))
}

// PrefixSHA is the hash of a call's message list but its last message: equal for two calls that differ only in what
// was added at the end, which is what offline replay at the same prefix keys on.
func PrefixSHA(messageSHAs []string) string {
	if len(messageSHAs) == 0 {
		return PromptSHA(nil)
	}
	return PromptSHA(messageSHAs[:len(messageSHAs)-1])
}

// orUntagged is p, or PurposeUntagged for none.
func orUntagged(p string) string {
	if p == "" {
		return PurposeUntagged
	}
	return p
}

// withholder withholds every string of a decoded JSON value: the desktop's marked secrets first, then what
// withhold.Text finds. An image sent as a data: URL is stored as a blob and replaced by a reference to it.
type withholder struct {
	secrets  []string
	blobs    *blobstore.Store
	problems []string
}

// value is v with every string withheld and every inline image stored.
func (w *withholder) value(v any) any {
	switch t := v.(type) {
	case string:
		return w.text(t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = w.value(e)
		}
		return out
	case map[string]any:
		if ref, ok := w.image(t); ok {
			return ref
		}
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = w.value(e)
		}
		return out
	}
	return v
}

// image stores an OpenAI image part whose URL is a base64 data: URL, and returns the part that replaces it.
func (w *withholder) image(part map[string]any) (map[string]any, bool) {
	if part["type"] != "image_url" {
		return nil, false
	}
	var url string
	switch iu := part["image_url"].(type) {
	case map[string]any:
		url, _ = iu["url"].(string)
	case string:
		url = iu
	}
	rest, ok := strings.CutPrefix(url, "data:")
	if !ok {
		return nil, false
	}
	meta, data, found := strings.Cut(rest, ",")
	mime, isBase64 := strings.CutSuffix(meta, ";base64")
	if !found || !isBase64 {
		w.problems = append(w.problems, "image not stored: not a base64 data URL")
		return map[string]any{"type": "image_ref", "sha256": "", "mime": mime}, true
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		w.problems = append(w.problems, "image not stored: bad base64")
		return map[string]any{"type": "image_ref", "sha256": "", "mime": mime}, true
	}
	sha, err := w.blobs.PutBytes(raw)
	if err != nil {
		w.problems = append(w.problems, "image not stored: "+err.Error())
		sha = ""
	}
	return map[string]any{"type": "image_ref", "sha256": sha, "mime": mime}, true
}

// text is s with the marked secrets and what withhold.Text finds withheld.
func (w *withholder) text(s string) string {
	for _, secret := range w.secrets {
		s = strings.ReplaceAll(s, secret, withhold.Marker)
	}
	return withhold.Text(s)
}

// longestFirst is the non-empty values, longest first, so a secret inside a longer one is not replaced first.
func longestFirst(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			out = append(out, v)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// dedupe is values without repeats, in order.
func dedupe(values []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

const rowColumns = `id, at_ns, ms, provider, deployment, status, desktop_id, task_id, turn, purpose, caller, messages,
	prefix_sha, prompt_sha, params, response_sha, input_tokens, cached_input_tokens, output_tokens, problems`

// scanRow reads one llm_calls row.
func scanRow(row sqlitedb.Scanner) (Row, error) {
	var r Row
	var at int64
	var messages, params, problems string
	var in, cached, out sql.NullInt64
	if err := row.Scan(&r.ID, &at, &r.MS, &r.Provider, &r.Deployment, &r.Status, &r.DesktopID, &r.TaskID, &r.Turn,
		&r.Purpose, &r.Caller, &messages, &r.PrefixSHA, &r.PromptSHA, &params, &r.ResponseSHA, &in, &cached, &out,
		&problems); err != nil {
		return r, err
	}
	r.At = sqlitedb.FromNS(at).UTC()
	_ = json.Unmarshal([]byte(messages), &r.Messages)
	_ = json.Unmarshal([]byte(problems), &r.Problems)
	if r.Messages == nil {
		r.Messages = []string{}
	}
	if r.Problems == nil {
		r.Problems = []string{}
	}
	r.Params = json.RawMessage(params)
	if !json.Valid(r.Params) {
		r.Params = json.RawMessage("{}")
	}
	for _, p := range []struct {
		from sql.NullInt64
		to   **int64
	}{{in, &r.InputTokens}, {cached, &r.CachedInputTokens}, {out, &r.OutputTokens}} {
		if p.from.Valid {
			v := p.from.Int64
			*p.to = &v
		}
	}
	return r, nil
}

// Calls is every call tagged with taskID, in the order they were made.
func (s *Store) Calls(taskID string) ([]Row, error) {
	return sqlitedb.QueryAll(s.db, scanRow, `SELECT `+rowColumns+` FROM llm_calls WHERE task_id = ? ORDER BY at_ns, id`,
		taskID)
}

// CallsForTask is the calls tagged with taskID that came from worker's desktop between from and to (either zero:
// open), in order: the rows a run record joins. A row tagged with the task from another desktop is never one of them.
func (s *Store) CallsForTask(taskID, worker string, from, to time.Time) ([]Row, error) {
	lo, hi := int64(0), int64(1<<62)
	if !from.IsZero() {
		lo = sqlitedb.ToNS(from)
	}
	if !to.IsZero() {
		hi = sqlitedb.ToNS(to)
	}
	return sqlitedb.QueryAll(s.db, scanRow, `SELECT `+rowColumns+` FROM llm_calls
		WHERE task_id = ? AND desktop_id = ? AND at_ns >= ? AND at_ns <= ? ORDER BY at_ns, id`, taskID, worker, lo, hi)
}

// DeploymentFor is the deployment that served task's calls from worker since since: the one with the most calls of
// purpose (when purpose is given and any such call exists), else the one with the most calls of any purpose. "" when
// no call of the task was recorded.
func (s *Store) DeploymentFor(taskID, worker string, since time.Time, purpose string) (string, error) {
	pick := func(withPurpose bool) (string, error) {
		q := `SELECT deployment FROM llm_calls WHERE task_id = ? AND desktop_id = ? AND at_ns >= ? AND deployment != ''`
		args := []any{taskID, worker, sqlitedb.ToNS(since)}
		if withPurpose {
			q += ` AND purpose = ?`
			args = append(args, purpose)
		}
		q += ` GROUP BY deployment ORDER BY COUNT(*) DESC, MAX(at_ns) DESC LIMIT 1`
		var d string
		err := s.db.QueryRow(q, args...).Scan(&d)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return d, err
	}
	if purpose != "" {
		if d, err := pick(true); err != nil || d != "" {
			return d, err
		}
	}
	return pick(false)
}

// Stats counts the stored calls by purpose, and the untagged and invalid ones.
func (s *Store) Stats() (Stats, error) {
	st := Stats{ByPurpose: map[string]int64{}, RecordFailures: s.Failures()}
	type count struct {
		purpose string
		n       int64
	}
	counts, err := sqlitedb.QueryAll(s.db, func(row sqlitedb.Scanner) (count, error) {
		var c count
		return c, row.Scan(&c.purpose, &c.n)
	}, `SELECT purpose, COUNT(*) FROM llm_calls GROUP BY purpose`)
	if err != nil {
		return st, err
	}
	for _, c := range counts {
		st.ByPurpose[c.purpose] = c.n
		st.Calls += c.n
	}
	st.Untagged = st.ByPurpose[PurposeUntagged]
	err = s.db.QueryRow(`SELECT COUNT(*) FROM llm_calls WHERE problems LIKE '%"invalid %'`).Scan(&st.Invalid)
	return st, err
}

// OpenBlob opens a stored message, answer, tool list or image by its hash.
func (s *Store) OpenBlob(sha string) (io.ReadCloser, error) {
	if !blobstore.ValidSHA(sha) {
		return nil, blobstore.ErrBadSHA
	}
	if !s.blobs.Has(sha) {
		return nil, fmt.Errorf("llmrec: no blob %s", sha)
	}
	return s.blobs.Open(sha)
}
