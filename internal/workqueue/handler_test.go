package workqueue

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func serve(t *testing.T, e *env) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	(&Handler{Store: e.store, Dispatcher: e.d}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func call(t *testing.T, srv *httptest.Server, method, path, body string, out any) int {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode
}

func TestHandlerEnqueueListCancelRetry(t *testing.T) {
	e := newEnv(t, Worker{ID: "w1"})
	srv := serve(t, e)

	body, _ := json.Marshal(map[string]any{"request": "summarise", "priority": 4, "required_role": "ops",
		"files": []map[string]any{{"name": "a.txt", "data": []byte("hello")}}, "source": "erp-hook"})
	var it Item
	if code := call(t, srv, "POST", "/api/queue", string(body), &it); code != http.StatusCreated {
		t.Fatalf("enqueue = %d", code)
	}
	if it.Status != StatusQueued || it.Priority != 4 || len(it.Files) != 1 || it.Files[0].Size != 5 {
		t.Fatalf("item = %+v", it)
	}

	var list struct{ Items []Item }
	if code := call(t, srv, "GET", "/api/queue?status=queued", "", &list); code != 200 || len(list.Items) != 1 {
		t.Fatalf("list = %d %+v", code, list)
	}
	if code := call(t, srv, "GET", "/api/queue?status=done", "", &list); code != 200 || len(list.Items) != 0 {
		t.Fatalf("filtered list = %d %+v", code, list)
	}
	var errBody map[string]string
	if code := call(t, srv, "GET", "/api/queue?status=bogus", "", &errBody); code != 400 {
		t.Fatalf("bad status = %d", code)
	}

	var c Item
	if code := call(t, srv, "POST", "/api/queue/cancel", `{"id":"`+it.ID+`","reason":"dup"}`, &c); code != 200 || c.Status != StatusCancelled {
		t.Fatalf("cancel = %d %+v", code, c)
	}
	if code := call(t, srv, "POST", "/api/queue/cancel", `{"id":"`+it.ID+`"}`, &errBody); code != http.StatusConflict {
		t.Fatalf("second cancel = %d", code)
	}
	if code := call(t, srv, "POST", "/api/queue/retry", `{"id":"`+it.ID+`"}`, &c); code != 200 || c.Status != StatusQueued {
		t.Fatalf("retry = %d %+v", code, c)
	}
	if code := call(t, srv, "POST", "/api/queue/retry", `{"id":"qmissing"}`, &errBody); code != 404 {
		t.Fatalf("retry missing = %d", code)
	}
	if code := call(t, srv, "GET", "/api/queue/cancel", "", &errBody); code != 405 {
		t.Fatalf("GET cancel = %d", code)
	}
}

func TestHandlerRejectsBadInputWithoutInternals(t *testing.T) {
	e := newEnv(t)
	srv := serve(t, e)
	var errBody map[string]string
	if code := call(t, srv, "POST", "/api/queue", `{"request":""}`, &errBody); code != 400 || errBody["error"] != "request is empty" {
		t.Fatalf("empty = %d %v", code, errBody)
	}
	if code := call(t, srv, "POST", "/api/queue", `{"request":"x","surprise":1}`, &errBody); code != 400 {
		t.Fatalf("unknown field = %d", code)
	}
	if code := call(t, srv, "POST", "/api/queue", `not json`, &errBody); code != 400 || strings.Contains(errBody["error"], "invalid character") {
		t.Fatalf("bad json = %d %v", code, errBody)
	}
}

func TestHandlerCapacityLimitsSpend(t *testing.T) {
	e := newEnv(t, Worker{ID: "w1"})
	srv := serve(t, e)

	var c Capacity
	if code := call(t, srv, "POST", "/api/queue/capacity", `{"worker":"w1","max_concurrent":2,"paused":true}`, &c); code != 200 || !c.Paused || c.MaxConcurrent != 2 {
		t.Fatalf("set capacity = %d %+v", code, c)
	}
	var caps struct{ Capacity []Capacity }
	if code := call(t, srv, "GET", "/api/queue/capacity", "", &caps); code != 200 || len(caps.Capacity) != 1 {
		t.Fatalf("capacity = %d %+v", code, caps)
	}

	if code := call(t, srv, "POST", "/api/queue/limits", `{"worker":"*","period":"day","max_usd":3}`, nil); code != 200 {
		t.Fatalf("set limit = %d", code)
	}
	if code := call(t, srv, "POST", "/api/queue/limits", `{"worker":"w1","period":"month","max_tokens":5000}`, nil); code != 200 {
		t.Fatalf("set limit = %d", code)
	}
	var lim struct{ Limits []Limit }
	if code := call(t, srv, "GET", "/api/queue/limits", "", &lim); code != 200 || len(lim.Limits) != 2 {
		t.Fatalf("limits = %d %+v", code, lim)
	}

	now := e.clock.now()
	e.spend.entries = []spendEntry{
		{"w1", now.Add(-time.Hour), Spend{USD: 1.5, Tokens: 300}},
		{"w1", time.Date(2026, 3, 1, 1, 0, 0, 0, time.UTC), Spend{USD: 2, Tokens: 700}},
		{"w1", time.Date(2026, 2, 1, 1, 0, 0, 0, time.UTC), Spend{USD: 50, Tokens: 9000}},
	}
	var sp struct {
		Worker string
		Day    struct {
			USD    float64
			Tokens int64
			Limit  *Limit
		}
		Month struct {
			USD    float64
			Tokens int64
			Limit  *Limit
		}
	}
	if code := call(t, srv, "GET", "/api/queue/spend?worker=w1", "", &sp); code != 200 {
		t.Fatalf("spend = %d", code)
	}
	if sp.Day.USD != 1.5 || sp.Month.USD != 3.5 || sp.Month.Tokens != 1000 {
		t.Fatalf("spend = %+v", sp)
	}
	if sp.Day.Limit == nil || sp.Day.Limit.Worker != "*" || sp.Month.Limit == nil || sp.Month.Limit.MaxTokens != 5000 {
		t.Fatalf("limits in spend = %+v / %+v", sp.Day.Limit, sp.Month.Limit)
	}
	var errBody map[string]string
	if code := call(t, srv, "GET", "/api/queue/spend?worker=", "", &errBody); code != 400 {
		t.Fatalf("no worker = %d", code)
	}

	// A limit with no cap removes it.
	if code := call(t, srv, "POST", "/api/queue/limits", `{"worker":"*","period":"day"}`, nil); code != 200 {
		t.Fatalf("remove limit = %d", code)
	}
	call(t, srv, "GET", "/api/queue/limits", "", &lim)
	if len(lim.Limits) != 1 {
		t.Fatalf("limits after removal = %+v", lim)
	}
}

func TestHandlerSpendWithoutDispatcher(t *testing.T) {
	e := newEnv(t)
	mux := http.NewServeMux()
	(&Handler{Store: e.store}).Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/queue/spend?worker=w1", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/queue", bytes.NewBufferString(`{"request":"x"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("enqueue without dispatcher = %d", rec.Code)
	}
}
