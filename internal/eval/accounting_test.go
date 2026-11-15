// Tests that token accounting follows the provider's reported usage, never the transcript's size: a fake
// OpenAI-compatible provider reports usage that disagrees with what was sent (inflated in one case, deflated in the
// other), and the broker's case and run counts, the case budget, the call recorder and the eval row all read the same
// reported figures, once each (docs/evals.md, "Bookkeeping").
package eval

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/blobstore"
	"github.com/YKoustubhRao/decurix/internal/llm"
	"github.com/YKoustubhRao/decurix/internal/llmrec"
	"github.com/YKoustubhRao/decurix/internal/practice"
	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// Usage the fake provider reports, whatever it was sent.
const (
	inflatedIn, inflatedCached, inflatedOut = 5000, 4000, 1000 // for a one-word transcript (a case budget is 5,000)
	deflatedIn, deflatedOut                 = 7, 3             // for a transcript of 200,000 characters
)

// usageProvider is a fake OpenAI-compatible server: a request whose transcript is long gets the deflated usage, a
// short one the inflated usage.
func usageProvider(t *testing.T) (*httptest.Server, *atomic.Int64) {
	var calls atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls.Add(1)
		in, cached, out := int64(inflatedIn), int64(inflatedCached), int64(inflatedOut)
		if len(body) > 100000 {
			in, cached, out = deflatedIn, 0, deflatedOut
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"model":"big-1-2026","choices":[{"message":{"role":"assistant","content":"ok"}}],`+
			`"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d,"prompt_tokens_details":{"cached_tokens":%d}}}`,
			in, out, in+out, cached)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// realHub is an llm.Hub whose azure provider is the fake server, with its database (for the spend meter).
func realHub(t *testing.T, srv *httptest.Server) (*llm.Hub, *sql.DB) {
	t.Helper()
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "acct.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	hub, err := llm.NewHub(db, key)
	if err != nil {
		t.Fatal(err)
	}
	hub.HTTP = srv.Client()
	if err := hub.Save("azure", llm.Settings{Endpoint: srv.URL, Key: "test-key", Model: "big-1", FastModel: "small-1"}); err != nil {
		t.Fatal(err)
	}
	return hub, db
}

// transcript is a chat body whose messages hold n characters.
func transcript(n int) string {
	b, _ := json.Marshal(map[string]any{"model": "default", "messages": []any{map[string]string{"role": "user", "content": strings.Repeat("w", n)}}})
	return string(b)
}

func recordedUsage(t *testing.T, db *sql.DB) (n int, in, cached, out int64) {
	t.Helper()
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(input_tokens),0), COALESCE(SUM(cached_input_tokens),0), COALESCE(SUM(output_tokens),0) FROM llm_calls`).
		Scan(&n, &in, &cached, &out); err != nil {
		t.Fatal(err)
	}
	return
}

// The broker, the case budget and the recorder count what the provider reported, inflated or deflated, once.
func TestAccountingFollowsTheProvidersReportedUsage(t *testing.T) {
	srv, upstream := usageProvider(t)
	hub, db := realHub(t, srv)
	blobs, err := blobstore.New(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	rec, err := llmrec.NewStore(db, blobs)
	if err != nil {
		t.Fatal(err)
	}
	b := &practice.Broker{LLM: hub, Recorder: rec}
	b.Begin("practice-acct-w", 0)

	post := func(body string) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/llm/azure/v1/chat/completions", strings.NewReader(body))
		r.Header.Set(llmrec.HeaderPurpose, "operator.turn")
		b.Handler().ServeHTTP(w, r)
		return w.Code
	}

	// Deflated: a 200,000-character transcript the provider says cost 7 + 3 tokens. A budget of 1,000 tokens is not
	// reached, however long the transcript.
	b.StartCase("t-deflated")
	b.LimitCase(1000)
	for i := 0; i < 3; i++ {
		if code := post(transcript(200000)); code != http.StatusOK {
			t.Fatalf("a deflated call was refused: %d", code)
		}
	}
	u := b.CaseUsage()
	if u.Input != 3*deflatedIn || u.Output != 3*deflatedOut || u.Cached != 0 || u.Calls != 3 {
		t.Fatalf("deflated case usage %+v, want the provider's 3 x (%d, %d)", u, deflatedIn, deflatedOut)
	}
	select {
	case <-b.CaseExhausted():
		t.Fatal("the case budget tripped on the transcript's size, not the reported usage")
	default:
	}
	b.EndCase()

	// Inflated: a one-word transcript the provider says cost 6,000 tokens. The budget trips after that one call, and
	// the next is refused without reaching the provider.
	b.StartCase("t-inflated")
	b.LimitCase(5500)
	if code := post(transcript(1)); code != http.StatusOK {
		t.Fatalf("the inflated call was refused: %d", code)
	}
	select {
	case <-b.CaseExhausted():
	default:
		t.Fatal("the case budget did not trip on the provider's reported usage")
	}
	before := upstream.Load()
	if code := post(transcript(1)); code != http.StatusTooManyRequests {
		t.Fatalf("a call past the budget answered %d", code)
	}
	if upstream.Load() != before {
		t.Fatal("a call past the budget reached the provider")
	}
	u = b.CaseUsage()
	if u.Input != inflatedIn || u.Cached != inflatedCached || u.Output != inflatedOut || u.Calls != 1 {
		t.Fatalf("inflated case usage %+v", u)
	}
	b.EndCase()

	// The run's count is the two cases' sum, each call once.
	want := int64(3*(deflatedIn+deflatedOut) + inflatedIn + inflatedOut)
	if got := b.Tokens(); got != want {
		t.Fatalf("run tokens %d, want %d", got, want)
	}
	// The recorder kept the same reported figures, one row per call that reached the provider.
	n, in, cached, out := recordedUsage(t, db)
	if n != 4 || in != 3*deflatedIn+inflatedIn || cached != inflatedCached || out != 3*deflatedOut+inflatedOut {
		t.Fatalf("recorded %d calls: in %d cached %d out %d", n, in, cached, out)
	}
	// The broker never meters a practice desktop's calls into the spend table: that is the proxy's (one place counts
	// a call against a spend limit, never two).
	if spent, err := hub.Metered("practice-acct-w", time.Now().Add(-time.Hour)); err != nil || spent != 0 {
		t.Fatalf("the broker metered practice calls: %d, %v", spent, err)
	}
	// The spend meter reads the same answer the same way (the proxy calls Meter with llm.ParseUsage of the answer).
	answer := fmt.Sprintf(`{"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":1}}`, inflatedIn, inflatedOut)
	if err := hub.Meter("desk-1", time.Now(), llm.ParseUsage([]byte(answer))); err != nil {
		t.Fatal(err)
	}
	if spent, _ := hub.Metered("desk-1", time.Now().Add(-time.Hour)); spent != inflatedIn+inflatedOut {
		t.Fatalf("metered %d, want the reported %d (total_tokens is not added on top)", spent, inflatedIn+inflatedOut)
	}
}

// An eval row's tokens are the provider's reported usage of its case's calls, and the matrix trips the case budget on
// it: a short transcript reported as huge ends the case at its budget; a huge one reported as tiny does not.
func TestEvalRowsCountTheProvidersReportedUsage(t *testing.T) {
	srv, _ := usageProvider(t)
	hub, db := realHub(t, srv)
	blobs, err := blobstore.New(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	rec, err := llmrec.NewStore(db, blobs)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := &practice.Broker{LLM: hub, Recorder: rec}
	call := func(fs *fakeSandbox, n int) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/llm/azure/chat", strings.NewReader(transcript(n)))
		r.Header.Set(llmrec.HeaderPurpose, "operator.turn")
		fs.broker.Handler().ServeHTTP(w, r)
	}
	worker := func(fs *fakeSandbox, caseID, _ string, _ int) {
		switch caseID {
		case "office": // a long transcript, reported tiny: twice, then done
			call(fs, 200000)
			call(fs, 200000)
			fs.save("office", "out.txt", "3\n")
			fs.final("Totalled.", true)
		case "web-save": // a short transcript reported huge: the case's budget (5,000) trips after one call
			call(fs, 1)
		}
	}
	sb := newSandbox(t, b, worker)
	s := &Service{Store: store, Sandbox: sb, Broker: b, BrokerURL: func(context.Context) (string, error) { return "http://172.30.0.1:8091", nil },
		Hub: hub, Recorder: rec, Practice: &practice.Service{}, FreeMemory: func() (uint64, error) { return 8 << 30, nil },
		BootTimeout: time.Minute, Minute: 2 * time.Second}
	run, err := s.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 1, Cases: []string{"web-save", "office"},
		Arms: []Arm{{Name: "default", Kind: ArmModel}}})
	if err != nil {
		t.Fatal(err)
	}
	run = waitDone(t, s, run.ID)
	rows, _ := s.Store.Rows(run.ID)
	if len(rows) != 2 {
		t.Fatalf("%d rows", len(rows))
	}
	for _, r := range rows {
		switch r.CaseID {
		case "office":
			if r.TokensIn != 2*deflatedIn || r.TokensOut != 2*deflatedOut || r.Calls != 2 || !r.Passed {
				t.Fatalf("office row: %+v", r)
			}
		case "web-save":
			if r.Observed != OutcomeBudget || r.TokensIn != inflatedIn || r.TokensCached != inflatedCached || r.TokensOut != inflatedOut || r.Calls != 1 {
				t.Fatalf("web-save row: %+v", r)
			}
		}
	}
	// The recorder holds the same calls with the same figures: nothing counted twice between the row and the record.
	n, in, _, out := recordedUsage(t, db)
	if n != 3 || in != 2*deflatedIn+inflatedIn || out != 2*deflatedOut+inflatedOut {
		t.Fatalf("recorded %d calls: in %d out %d", n, in, out)
	}
}
