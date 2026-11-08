package practice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/llm"
	"github.com/YKoustubhRao/decurix/internal/llmrec"
)

// deployingChat is a hub that says which deployment served each call, by the label the call asked for.
type deployingChat struct {
	mu     sync.Mutex
	bodies []string
}

func (d *deployingChat) Configured(string) bool { return true }
func (d *deployingChat) Chat(ctx context.Context, p string, body []byte) (int, []byte, http.Header, error) {
	r, err := d.ChatResult(ctx, p, body)
	return r.Status, r.Body, r.Retry, err
}
func (d *deployingChat) ChatResult(_ context.Context, _ string, body []byte) (llm.ChatResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.bodies = append(d.bodies, string(body))
	var req struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &req)
	dep := map[string]string{"default": "big-1", "fast": "small-1"}[req.Model]
	return llm.ChatResult{Status: 200, Deployment: dep,
		Body: []byte(`{"choices":[],"usage":{"prompt_tokens":300,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":200}}}`)}, nil
}

type callLog struct {
	mu    sync.Mutex
	calls []llmrec.Call
}

func (c *callLog) Record(_ context.Context, call llmrec.Call) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, call)
}

func brokerPost(b *Broker, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	b.Handler().ServeHTTP(w, r)
	return w
}

func TestTheBrokerPassesOnEveryTurnOfACase(t *testing.T) {
	b := &Broker{LLM: &deployingChat{}, Caller: desktopAt("practice-r1")}
	b.Begin("practice-r1", 0)
	obs := b.StartCase("t1")
	brokerPost(b, "/api/querytext", `{"desktop_id":"practice-r1","kind":"query","reply":"Which ledger?"}`, nil)
	brokerPost(b, "/api/querytext", `{"desktop_id":"practice-r1","kind":"query","reply":"The password?","secret":true}`, nil)
	brokerPost(b, "/api/querytext", `{"desktop_id":"practice-r1","kind":"approval","reply":"May I?","gate":{"id":"g1","question":"Click \"Pay\"?","sig":"s"}}`, nil)
	brokerPost(b, "/api/querytext", `{"desktop_id":"practice-r1","kind":"final","reply":"Done.","checked":true,"verdict":{"schema":"decurix.verdict/v1","verdict":"done"}}`, nil)
	var got []Observation
	for len(got) < 4 {
		got = append(got, <-obs)
	}
	if got[0].Kind != "query" || got[0].Secret || got[1].Kind != "query" || !got[1].Secret {
		t.Fatalf("questions: %+v", got[:2])
	}
	if got[2].Kind != "approval" || got[2].Gate != `Click "Pay"?` || got[2].Text != "May I?" {
		t.Fatalf("approval: %+v", got[2])
	}
	if got[3].Kind != "final" || !got[3].Checked || !strings.Contains(string(got[3].Verdict), `"done"`) {
		t.Fatalf("final: %+v", got[3])
	}
	// The broker answered nothing: each callback got only its receipt.
	if w := brokerPost(b, "/api/querytext", `{"desktop_id":"practice-r1","kind":"query","reply":"?"}`, nil); w.Body.String() != `{"status":"received"}` {
		t.Fatalf("the broker answered: %s", w.Body.String())
	}
}

func TestTheBrokerServesTheRunsModelAndCountsTheCase(t *testing.T) {
	chat := &deployingChat{}
	rec := &callLog{}
	b := &Broker{LLM: chat, Recorder: rec}
	b.BeginModel("practice-r1", 0, "fast")
	b.StartCase("t1")
	b.LimitCase(600)
	headers := map[string]string{llmrec.HeaderPurpose: "operator.turn", llmrec.HeaderTurn: "j1:3", llmrec.HeaderCaller: "soul"}
	body := `{"model":"default","messages":[{"role":"user","content":"x"}]}`
	if w := brokerPost(b, "/api/llm/azure/chat", body, headers); w.Code != 200 {
		t.Fatalf("call: %d", w.Code)
	}
	if !strings.Contains(chat.bodies[0], `"model":"fast"`) || !strings.Contains(chat.bodies[0], `"content":"x"`) {
		t.Fatalf("served: %s", chat.bodies[0])
	}
	u := b.CaseUsage()
	if u.Input != 300 || u.Cached != 200 || u.Output != 20 || u.Calls != 1 || len(u.Deployments) != 1 || u.Deployments[0] != "small-1" {
		t.Fatalf("usage: %+v", u)
	}
	if len(rec.calls) != 1 || rec.calls[0].Caller != llmrec.CallerPractice || rec.calls[0].Purpose != "operator.turn" || rec.calls[0].Turn != "j1:3" ||
		rec.calls[0].Task != "t1" || rec.calls[0].Desktop != "practice-r1" || rec.calls[0].Deployment != "small-1" {
		t.Fatalf("recorded: %+v", rec.calls)
	}
	// A second call takes the case past its 600 tokens: the case is over and the next call is refused.
	brokerPost(b, "/api/llm/azure/chat", body, nil)
	select {
	case <-b.CaseExhausted():
	default:
		t.Fatal("the case's budget was not reached")
	}
	if w := brokerPost(b, "/api/llm/azure/chat", body, nil); w.Code != http.StatusTooManyRequests {
		t.Fatalf("a call past the case's budget: %d", w.Code)
	}
	// A new case starts from nothing.
	b.StartCase("t2")
	if u := b.CaseUsage(); u.Calls != 0 || u.Input != 0 || len(u.Deployments) != 0 {
		t.Fatalf("usage carried over: %+v", u)
	}
	if w := brokerPost(b, "/api/llm/azure/chat", `{"model":"other-name","messages":[]}`, nil); w.Code != 200 || !strings.Contains(chat.bodies[len(chat.bodies)-1], `"model":"other-name"`) {
		t.Fatal("a call that names its own model was changed")
	}
}

// throttlingChat answers in turn: each status with a body that names a model only when it succeeds.
type throttlingChat struct {
	mu       sync.Mutex
	statuses []int
}

func (c *throttlingChat) Configured(string) bool { return true }
func (c *throttlingChat) Chat(ctx context.Context, p string, body []byte) (int, []byte, http.Header, error) {
	r, err := c.ChatResult(ctx, p, body)
	return r.Status, r.Body, r.Retry, err
}
func (c *throttlingChat) ChatResult(context.Context, string, []byte) (llm.ChatResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	status := c.statuses[0]
	c.statuses = c.statuses[1:]
	if status != 200 {
		return llm.ChatResult{Status: status, Deployment: "big-1", Body: []byte(`{"error":{"code":"429","message":"Rate limit is exceeded."}}`)}, nil
	}
	return llm.ChatResult{Status: 200, Deployment: "big-1", Body: []byte(`{"model":"gpt-big-2026-01","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":1}}`)}, nil
}

// A throttled or failed answer names no model that served anything: only a 2xx answer's model is recorded
// (review EVAL-3, R3-B1).
func TestOnlyASuccessfulAnswerSaysWhichModelServed(t *testing.T) {
	for name, statuses := range map[string][]int{"429 between": {200, 429, 200}, "429 first": {429, 200}, "500 only": {500}} {
		b := &Broker{LLM: &throttlingChat{statuses: statuses}}
		b.Begin("practice-r1", 0)
		b.StartCase("t1")
		for range statuses {
			brokerPost(b, "/api/llm/azure/chat", `{"model":"default","messages":[]}`, nil)
		}
		served := b.CaseUsage().Served
		want := 1
		if name == "500 only" {
			want = 0
		}
		if len(served) != want || (want == 1 && served[0] != "gpt-big-2026-01") {
			t.Fatalf("%s: served %q", name, served)
		}
	}
}
