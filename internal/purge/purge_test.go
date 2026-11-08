// purge_test.go plants a value in every store a purge covers, purges it, and checks it is gone everywhere while
// every row still joins its blobs, and that the purge is recorded without the value.

package purge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/blobstore"
	"github.com/YKoustubhRao/decurix/internal/concierge"
	"github.com/YKoustubhRao/decurix/internal/conversation"
	"github.com/YKoustubhRao/decurix/internal/llmrec"
	"github.com/YKoustubhRao/decurix/internal/querynotes"
	"github.com/YKoustubhRao/decurix/internal/runs"
	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// leaked is a value nothing withholds by its shape: only a purge takes it out.
const leaked = "lanternmossquietfield"

var owner = tasks.LocalOwner

// testStores opens every store on one in-memory database and one blob store.
func testStores(t *testing.T) Stores {
	t.Helper()
	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	blobs, err := blobstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st := Stores{DB: db, Blobs: blobs}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	st.Calls, err = llmrec.NewStore(db, blobs)
	must(err)
	st.Tasks, err = tasks.NewStore(db)
	must(err)
	st.Chat, err = conversation.NewStore(db)
	must(err)
	st.Concierge, err = concierge.NewStore(db)
	must(err)
	st.Notes, err = querynotes.NewStore(db)
	must(err)
	st.Runs, err = runs.NewStore(db, blobs)
	must(err)
	return st
}

// plant puts leaked into every store, and returns the blob a bundle file is kept in.
func plant(t *testing.T, st Stores) string {
	t.Helper()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := st.DB.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	now := time.Now().UnixNano()
	// A model call: the value in a message, in a tool call's arguments (JSON inside JSON), and in the answer.
	req, _ := json.Marshal(map[string]any{"model": "m", "messages": []any{
		map[string]any{"role": "system", "content": "You are a worker."},
		map[string]any{"role": "user", "content": "log in with " + leaked + " please"},
		map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"function": map[string]any{
			"name": "type", "arguments": `{"text":"` + leaked + `\n"}`}}}},
	}})
	resp, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "typed " + leaked}}}})
	st.Calls.Record(context.Background(), llmrec.Call{Provider: "azure", Desktop: "d1", Task: "t1", Purpose: "agent.turn",
		Request: req, Response: resp, Status: 200, Problems: []string{"note: " + leaked}})
	// A run bundle whose file holds the value, split far from the start so the streamed rewrite crosses a chunk.
	file := bytes.Repeat([]byte("x"), 64<<10-5)
	file = append(file, []byte(leaked+" and the rest")...)
	sha, n, err := st.Blobs.Put(bytes.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Runs.SaveBundle(runs.Bundle{ExecID: "x1", TaskID: "t1", Worker: "d1",
		Files: []runs.File{{Path: "runs/t1/log.txt", SHA256: sha, Bytes: n}}, Missing: []string{"screen: " + leaked}}); err != nil {
		t.Fatal(err)
	}
	// Task records, written as the store writes them.
	exec(`INSERT INTO tasks (id, worker_id, request, task_type, status, created_ns, updated_ns) VALUES ('t1', 'd1', ?, 'routine', 'done', ?, ?)`,
		"sign in as mira with "+leaked, now, now)
	exec(`INSERT INTO executions (id, task_id, attempt, worker_id, status, started_ns, final_reply) VALUES ('x1', 't1', 1, 'd1', 'done', ?, ?)`,
		now, "done; used "+leaked)
	exec(`INSERT INTO execution_steps (id, execution_id, text, created_ns) VALUES ('s1', 'x1', ?, ?)`, `$ login --pw "`+leaked+`"`, now)
	exec(`INSERT INTO interventions (id, execution_id, kind, reason, request, reply, requested_ns) VALUES ('i1', 'x1', 'query', 'needs', 'password?', ?, ?)`,
		leaked, now)
	if _, err := st.Tasks.AddJudgement(tasks.Judgement{Actor: owner, Kind: tasks.JudgementAnswer, TaskID: "t1", Text: "it is " + leaked,
		Payload: json.RawMessage(`{"said":"` + leaked + `"}`)}); err != nil {
		t.Fatal(err)
	}
	// The chat, in the database and in memory.
	st.Chat.AppendUserRequest("d1", "t1", "use "+leaked)
	st.Chat.AppendStep("d1", "typing "+leaked)
	if _, err := st.Concierge.AddMessage("user", 0, "", "tell d1 the code is "+leaked); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO query_notes (id, desktop_id, vm_name, filename, content, created_ns) VALUES ('q1', 'd1', 'vm', 'q.txt', ?, ?)`,
		"is "+leaked+" right?", now)
	return sha
}

// holdsValue names every place value is still found: each text column of the database, and each blob file.
func holdsValue(t *testing.T, st Stores, value string) []string {
	t.Helper()
	var found []string
	tables := map[string][]string{
		"llm_calls": {"params", "problems"}, "run_bundles": {"files", "missing", "history"},
		"tasks": {"request", "verdict_note"}, "executions": {"final_reply", "verdict"},
		"execution_steps": {"text", "action"}, "interventions": {"reason", "request", "reply"},
		"judgements": {"text", "payload", "changed"}, "chat_entries": {"content"}, "chat_steps": {"text", "action"},
		"concierge_messages": {"text"}, "query_notes": {"content", "gate_request", "gate_question", "resolution"},
	}
	for table, cols := range tables {
		for _, c := range cols {
			var n int
			if err := st.DB.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE instr(`+c+`, ?) > 0`, value).Scan(&n); err != nil {
				t.Fatalf("%s.%s: %v", table, c, err)
			}
			if n > 0 {
				found = append(found, table+"."+c)
			}
		}
	}
	_ = filepath.WalkDir(filepath.Join(st.Blobs.Root(), "sha256"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(value)) {
				found = append(found, "blob "+d.Name())
			}
		}
		return nil
	})
	snap := st.Chat.Snapshot("d1")
	b, _ := json.Marshal(snap)
	if bytes.Contains(b, []byte(value)) {
		found = append(found, "chat in memory")
	}
	return found
}

// A value planted in every store is gone from all of them after a purge, every row still opens its blobs at their
// recorded lengths, and the purge is a judgement with the counts and the reason, never the value.
func TestAPurgeTakesTheValueOutOfEveryStoreAndKeepsTheJoins(t *testing.T) {
	st := testStores(t)
	bundleSHA := plant(t, st)
	if before := holdsValue(t, st, leaked); len(before) < 12 {
		t.Fatalf("the value was planted in only %v: the test would prove nothing", before)
	}
	calls, err := st.Calls.Calls("t1")
	if err != nil || len(calls) != 1 {
		t.Fatalf("calls: %v %v", calls, err)
	}
	call := calls[0]

	res, err := st.Purge(context.Background(), leaked, "typed into a request by mistake", owner, "privacy")
	if err != nil {
		t.Fatal(err)
	}
	if left := holdsValue(t, st, leaked); len(left) > 0 {
		t.Fatalf("the value is still in %v", left)
	}
	c := res.Counts
	if c.Blobs < 3 || c.LLMCalls != 1 || c.RunBundles != 1 || c.TaskRecords != 4 || c.Judgements != 1 || c.Chat < 3 ||
		c.QueryNotes != 1 || c.BlobsFailed != 0 {
		t.Errorf("counts = %+v", c)
	}
	if len(res.Problems) != 0 {
		t.Errorf("problems: %v", res.Problems)
	}

	// Joins: the call row still names the same blobs, and each opens; the prefix still matches its messages.
	after, _ := st.Calls.Calls("t1")
	if len(after) != 1 || after[0].ID != call.ID || after[0].PrefixSHA != call.PrefixSHA ||
		strings.Join(after[0].Messages, ",") != strings.Join(call.Messages, ",") || after[0].ResponseSHA != call.ResponseSHA {
		t.Fatalf("the row changed its joins: %+v -> %+v", call, after)
	}
	if after[0].PrefixSHA != llmrec.PrefixSHA(after[0].Messages) {
		t.Error("the prefix no longer matches the messages")
	}
	for _, sha := range append(append([]string{}, after[0].Messages...), after[0].ResponseSHA) {
		rc, err := st.Calls.OpenBlob(sha)
		if err != nil {
			t.Fatalf("blob %s does not open: %v", sha, err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		if !json.Valid(b) {
			t.Errorf("blob %s is not JSON after the purge: %s", sha, b)
		}
	}
	// The bundle still lists its file at its length, the file opens, holds the mark, and is noted as purged.
	bundle, ok, err := st.Runs.Bundle("x1")
	if err != nil || !ok || len(bundle.Files) != 1 || bundle.Files[0].SHA256 != bundleSHA {
		t.Fatalf("bundle: %+v %v %v", bundle, ok, err)
	}
	rc, err := st.Blobs.Open(bundleSHA)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if int64(len(b)) != bundle.Files[0].Bytes || !bytes.Contains(b, []byte(Mark)) {
		t.Errorf("the bundle file is %d bytes (listed %d), mark %v", len(b), bundle.Files[0].Bytes, bytes.Contains(b, []byte(Mark)))
	}
	note, ok := st.Blobs.Purged(bundleSHA)
	if !ok || note.ContentSHA256 != blobstore.SHA256(b) || note.Judgement != res.Judgement {
		t.Errorf("purge note = %+v, %v", note, ok)
	}

	// The judgement: who, why, the counts; nowhere the value.
	j, ok, err := st.Tasks.Judgement(res.Judgement)
	if err != nil || !ok {
		t.Fatalf("judgement %s: %v %v", res.Judgement, ok, err)
	}
	if j.Kind != tasks.JudgementPurge || j.Actor.ID != owner.ID || j.Text != "typed into a request by mistake" ||
		!bytes.Contains(j.Payload, []byte(`"llm_calls":1`)) {
		t.Errorf("judgement = %+v", j)
	}
	out, _ := json.Marshal(res)
	jb, _ := json.Marshal(j)
	if bytes.Contains(out, []byte(leaked)) || bytes.Contains(jb, []byte(leaked)) {
		t.Error("the value is in the result or the judgement")
	}
}

// A reason that carries the value is kept with the value purged from it.
func TestTheReasonNeverKeepsTheValue(t *testing.T) {
	st := testStores(t)
	res, err := st.Purge(context.Background(), leaked, "mira pasted "+leaked+" into chat", owner, "privacy")
	if err != nil {
		t.Fatal(err)
	}
	j, _, _ := st.Tasks.Judgement(res.Judgement)
	if strings.Contains(j.Text, leaked) || !strings.Contains(j.Text, Mark) {
		t.Errorf("reason kept as %q", j.Text)
	}
}

// Refusals change nothing: a value of 5 characters, no reason, an actor who is not a person, another purge running.
func TestRefusalsChangeNothing(t *testing.T) {
	st := testStores(t)
	if _, err := st.DB.Exec(`INSERT INTO query_notes (id, desktop_id, vm_name, filename, content, created_ns) VALUES ('q1', 'd1', 'vm', 'f', 'code abcde here', 1)`); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		value, reason string
		actor         tasks.Actor
		want          error
	}{
		{"abcde", "leaked", owner, ErrTooShort},
		{"  abcde \n", "leaked", owner, ErrTooShort},
		{leaked, "  ", owner, ErrNoReason},
		{leaked, "leaked", tasks.Actor{ID: "d1", Kind: tasks.ActorWorker}, ErrNoPerson},
		{"purged", "leaked", owner, ErrIsMark},
	}
	for _, c := range cases {
		if _, err := st.Purge(context.Background(), c.value, c.reason, c.actor, "privacy"); !errors.Is(err, c.want) {
			t.Errorf("Purge(%q, %q, %s) = %v, want %v", c.value, c.reason, c.actor.Kind, err, c.want)
		}
	}
	if strings.Contains(ErrTooShort.Error(), "abcde") {
		t.Error("the refusal names the value")
	}
	var content string
	_ = st.DB.QueryRow(`SELECT content FROM query_notes WHERE id = 'q1'`).Scan(&content)
	if content != "code abcde here" {
		t.Errorf("a refused purge changed a note: %q", content)
	}
	running.Lock()
	_, err := st.Purge(context.Background(), leaked, "leaked", owner, "privacy")
	running.Unlock()
	if !errors.Is(err, ErrBusy) {
		t.Errorf("a second purge: %v, want ErrBusy", err)
	}
	if _, err := (Stores{}).Purge(context.Background(), leaked, "leaked", owner, "privacy"); !errors.Is(err, ErrNoRecord) {
		t.Errorf("no task records: %v", err)
	}
	var n int
	_ = st.DB.QueryRow(`SELECT COUNT(*) FROM judgements`).Scan(&n)
	if n != 0 {
		t.Errorf("a refusal recorded %d judgements", n)
	}
}

// Forms covers the value as written, inside a JSON string, and as base64, longest first.
func TestFormsCoverEscapesAndBase64(t *testing.T) {
	forms := Forms(`pa"ss/wörd`)
	want := []string{`pa"ss/wörd`, `pa\"ss/wörd`, `pa\"ss/wörd`, "cGEic3Mvd8O2cmQ="}
	for _, w := range want {
		found := false
		for _, f := range forms {
			found = found || f == w
		}
		if !found {
			t.Errorf("no form %q in %q", w, forms)
		}
	}
	for i := 1; i < len(forms); i++ {
		if len(forms[i]) > len(forms[i-1]) {
			t.Errorf("forms not longest first: %q", forms)
		}
	}
}
