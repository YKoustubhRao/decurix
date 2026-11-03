package llmrec

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/blobstore"
	"github.com/YKoustubhRao/decurix/internal/llm"
	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

func testStore(t *testing.T) *Store {
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
	s, err := NewStore(db, blobs)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func chatBody(t *testing.T, msgs []any, extra map[string]any) []byte {
	t.Helper()
	req := map[string]any{"model": "default", "messages": msgs}
	for k, v := range extra {
		req[k] = v
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// blobFiles is every blob file stored, by path.
func blobFiles(t *testing.T, s *Store) []string {
	t.Helper()
	var out []string
	_ = filepath.Walk(filepath.Join(s.blobs.Root(), "sha256"), func(p string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// Two calls that share their first ten messages store eleven message blobs, not twenty, and have the same prefix.
func TestMessagesAreStoredOnceAndTheirPrefixIsShared(t *testing.T) {
	s := testStore(t)
	var msgs []any
	for i := 0; i < 10; i++ {
		msgs = append(msgs, map[string]any{"role": "user", "content": fmt.Sprintf("message %d", i)})
	}
	// The first call is the ten; the second is the ten plus one: each call's prefix is all but its last message.
	s.Record(context.Background(), Call{Provider: "azure", Desktop: "eve", Task: "t1", Purpose: "operator.turn",
		Request:  chatBody(t, append(append([]any{}, msgs...), map[string]any{"role": "assistant", "content": "x"}), nil),
		Response: []byte(`{"choices":[]}`), Status: 200})
	s.Record(context.Background(), Call{Provider: "azure", Desktop: "eve", Task: "t1", Purpose: "operator.turn",
		Request:  chatBody(t, append(append([]any{}, msgs...), map[string]any{"role": "assistant", "content": "y"}), nil),
		Response: []byte(`{"choices":[]}`), Status: 200})

	rows, err := s.Calls("t1")
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows = %v, %v", rows, err)
	}
	if rows[0].PrefixSHA != rows[1].PrefixSHA {
		t.Fatal("two calls with the same first ten messages have different prefixes")
	}
	if rows[0].PromptSHA == rows[1].PromptSHA {
		t.Fatal("two different calls have the same prompt hash")
	}
	distinct := map[string]bool{}
	for _, r := range rows {
		for _, m := range r.Messages {
			distinct[m] = true
		}
	}
	if len(distinct) != 12 { // ten shared + "x" + "y"
		t.Fatalf("%d distinct message blobs, want 12", len(distinct))
	}
	// Twelve message blobs and one answer blob: nothing stored twice.
	if n := len(blobFiles(t, s)); n != 13 {
		t.Fatalf("%d blob files, want 13", n)
	}
}

// An image sent twice inline is stored once, by its bytes, and the stored message names it.
func TestAnInlineImageIsOneBlob(t *testing.T) {
	s := testStore(t)
	png := []byte("\x89PNG fake picture bytes")
	url := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	msg := func(text string) []any {
		return []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": text},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}},
		}}}
	}
	s.Record(context.Background(), Call{Provider: "azure", Purpose: "review", Task: "t2", Request: chatBody(t, msg("one"), nil), Status: 200})
	s.Record(context.Background(), Call{Provider: "azure", Purpose: "review", Task: "t2", Request: chatBody(t, msg("two"), nil), Status: 200})

	if !s.blobs.Has(blobstore.SHA256(png)) {
		t.Fatal("the image's bytes are not a blob")
	}
	rows, _ := s.Calls("t2")
	r, _ := s.OpenBlob(rows[0].Messages[0])
	var stored map[string]any
	_ = json.NewDecoder(r).Decode(&stored)
	r.Close()
	b, _ := json.Marshal(stored)
	if strings.Contains(string(b), "base64") || !strings.Contains(string(b), blobstore.SHA256(png)) {
		t.Fatalf("stored message %s", b)
	}
	count := 0
	for _, p := range blobFiles(t, s) {
		data, _ := os.ReadFile(p)
		if string(data) == string(png) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("the image is stored %d times", count)
	}
}

// A value the person marked secret, and one withhold.Text finds, are in no stored blob or row.
func TestSecretsReachNoBlob(t *testing.T) {
	s := testStore(t)
	var marked SecretSet
	marked.Remember("eve", "Zq9-plum-Orbit")
	body := chatBody(t, []any{
		map[string]any{"role": "user", "content": "log in as ravi, the code word is Zq9-plum-Orbit"},
		map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"function": map[string]any{
			"name": "shell", "arguments": `{"cmd":"gui type 'pw' 'Zq9-plum-Orbit'"}`}}}},
		map[string]any{"role": "user", "content": "the password: Hunter2Hunter2"},
	}, map[string]any{"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "shell"}}}})
	s.Record(context.Background(), Call{Provider: "azure", Desktop: "eve", Task: "t3", Purpose: "operator.turn",
		Request: body, Response: []byte(`{"choices":[{"message":{"content":"typed Zq9-plum-Orbit"}}]}`), Status: 200,
		Secrets: marked.For("eve")})

	files := blobFiles(t, s)
	if len(files) == 0 {
		t.Fatal("nothing stored")
	}
	for _, p := range files {
		data, _ := os.ReadFile(p)
		for _, secret := range []string{"Zq9-plum-Orbit", "Hunter2Hunter2"} {
			if strings.Contains(string(data), secret) {
				t.Fatalf("blob %s holds %q: %s", filepath.Base(p), secret, data)
			}
		}
	}
	rows, _ := s.Calls("t3")
	if b, _ := json.Marshal(rows); strings.Contains(string(b), "Zq9") {
		t.Fatalf("a row holds the secret: %s", b)
	}
	if !strings.Contains(string(rows[0].Params), "blob_ref") {
		t.Fatalf("tools not stored as a blob: %s", rows[0].Params)
	}
}

func TestRowsCarryUsageStatusAndTags(t *testing.T) {
	s := testStore(t)
	at := time.Now().Add(-time.Minute)
	s.Record(context.Background(), Call{Provider: "azure", Deployment: "big-1", Desktop: "eve", Task: "t4",
		Turn: "j1:3", Purpose: "operator.turn", Caller: "soul", Request: chatBody(t, nil, nil), Status: 429,
		Latency: 1500 * time.Millisecond, At: at, Usage: llm.Usage{Input: 100, Cached: 60, Output: 7}})
	s.Record(context.Background(), Call{Provider: "azure", Desktop: "eve", Task: "t4", Request: []byte("not json"),
		Status: 502})

	rows, _ := s.Calls("t4")
	if len(rows) != 2 {
		t.Fatalf("rows = %d", len(rows))
	}
	r := rows[0]
	if r.Deployment != "big-1" || r.Status != 429 || r.Turn != "j1:3" || r.Caller != "soul" || r.MS != 1500 ||
		*r.InputTokens != 100 || *r.CachedInputTokens != 60 || *r.OutputTokens != 7 || !r.At.Equal(at.UTC().Truncate(0)) {
		t.Fatalf("row = %+v", r)
	}
	if rows[1].Purpose != PurposeUntagged || rows[1].InputTokens != nil || len(rows[1].Problems) != 1 {
		t.Fatalf("an untagged, unparsed call = %+v", rows[1])
	}

	from := s.now().Add(-2 * time.Minute)
	if got, _ := s.CallsForTask("t4", "eve", from, time.Time{}); len(got) != 2 {
		t.Fatalf("CallsForTask = %d rows", len(got))
	}
	if got, _ := s.CallsForTask("t4", "mallory", time.Time{}, time.Time{}); len(got) != 0 {
		t.Fatal("another desktop's rows were joined")
	}
	st, err := s.Stats()
	if err != nil || st.Calls != 2 || st.Untagged != 1 || st.ByPurpose["operator.turn"] != 1 {
		t.Fatalf("stats = %+v, %v", st, err)
	}
}

func TestDeploymentForPrefersTheOperatorsTurns(t *testing.T) {
	s := testStore(t)
	since := time.Now().Add(-time.Hour)
	rec := func(dep, purpose string) {
		s.Record(context.Background(), Call{Provider: "azure", Deployment: dep, Desktop: "eve", Task: "t5",
			Purpose: purpose, Request: chatBody(t, nil, nil), Status: 200})
	}
	if d, _ := s.DeploymentFor("t5", "eve", since, "operator.turn"); d != "" {
		t.Fatalf("no calls gave %q", d)
	}
	rec("small-1", "review")
	rec("small-1", "review")
	rec("small-1", "review")
	if d, _ := s.DeploymentFor("t5", "eve", since, "operator.turn"); d != "small-1" {
		t.Fatalf("with no operator turns: %q", d)
	}
	rec("big-1", "operator.turn")
	if d, _ := s.DeploymentFor("t5", "eve", since, "operator.turn"); d != "big-1" {
		t.Fatalf("with one operator turn: %q", d)
	}
}

// A store whose database is gone still never fails the call: it counts the failure.
func TestARecordingFailureIsCountedNotRaised(t *testing.T) {
	s := testStore(t)
	s.db.Close()
	s.Record(context.Background(), Call{Provider: "azure", Purpose: "x", Request: chatBody(t, nil, nil), Status: 200})
	if s.Failures() != 1 {
		t.Fatalf("failures = %d", s.Failures())
	}
}

func TestSecretSetKeepsValuesAndForgets(t *testing.T) {
	now := time.Now()
	set := SecretSet{now: func() time.Time { return now }}
	set.Remember("eve", "  Orbit9 ", "ab", "Orbit9", "Plum-7")
	if got := strings.Join(set.For("eve"), "|"); got != "Orbit9|Plum-7" {
		t.Fatalf("held %q (a value under 3 characters is never held, a repeat once)", got)
	}
	if len(set.For("other")) != 0 {
		t.Fatal("another desktop's values leaked")
	}
	for i := 0; i < 100; i++ {
		set.Remember("eve", fmt.Sprintf("value-%03d", i))
	}
	if n := len(set.For("eve")); n != secretsPerDesktop {
		t.Fatalf("held %d values, want %d", n, secretsPerDesktop)
	}
	now = now.Add(25 * time.Hour)
	if n := len(set.For("eve")); n != 0 {
		t.Fatalf("held %d values a day later", n)
	}
}

// A held value is withheld also as it reads inside a JSON string (a tool call's arguments): with its quote and
// backslash escaped, and with a non-ASCII letter written as \uXXXX.
func TestAHeldValueIsWithheldInsideJSONStrings(t *testing.T) {
	values := []string{`Qz"9-lamp\Hx`, "pässwört-77"}
	asciiArgs := `{"command":"gui type e3 p\u00e4ssw\u00f6rt-77"}`
	goArgs, _ := json.Marshal(map[string]string{"command": "gui type e3 " + values[0]})
	for _, in := range []string{string(goArgs), asciiArgs, "typed " + values[0] + " and " + values[1]} {
		out := WithholdValues(in, values)
		for _, frag := range []string{"9-lamp", "rt-77"} {
			if strings.Contains(out, frag) {
				t.Errorf("%s -> %s keeps %q", in, out, frag)
			}
		}
	}
	if got := WithholdValues("the password field", values); got != "the password field" {
		t.Errorf("text with no held value changed: %q", got)
	}
}

// A held value inside a JSON string field is withheld by its parsed value, whatever escaping the writer used: an
// upper-case \u escape, an escaped slash, DEL as \u007f (review CP-A N3). A JSON string with nothing to withhold
// keeps its bytes.
func TestAHeldValueIsWithheldInsideParsedJSON(t *testing.T) {
	s := testStore(t)
	args := `{"command":"gui type e3 p\u00C4ss\/w\u007frd-77","note":"x"}`
	plain := `{"command": "gui click e4",  "n": 1}`
	body := chatBody(t, []any{map[string]any{"role": "assistant", "tool_calls": []any{
		map[string]any{"function": map[string]any{"name": "shell", "arguments": args}},
		map[string]any{"function": map[string]any{"name": "shell", "arguments": plain}},
	}}}, nil)
	s.Record(context.Background(), Call{Provider: "azure", Desktop: "eve", Task: "t9", Purpose: "operator.turn",
		Request: body, Status: 200, Secrets: []string{"pÄss/w\x7frd-77"}})
	for _, p := range blobFiles(t, s) {
		data, _ := os.ReadFile(p)
		if strings.Contains(string(data), "rd-77") {
			t.Fatalf("the value is in a blob: %s", data)
		}
		if strings.Contains(string(data), "gui click e4") && !strings.Contains(string(data), `\"gui click e4\",  \"n\": 1`) {
			t.Fatalf("a JSON string with nothing withheld was rewritten: %s", data)
		}
	}
}
