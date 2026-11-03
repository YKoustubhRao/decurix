package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"brainiac/internal/catalog"
	"brainiac/internal/config"
	"brainiac/internal/engine"
	"brainiac/internal/install"
	"brainiac/internal/selector"
	"brainiac/internal/store"
)

// replyEngine is an engine whose local brain answers reply. When gate is
// set, each answer waits for it, and started is told the answer began.
func replyEngine(t *testing.T, reply string, started chan<- struct{}, gate <-chan struct{}) *engine.Engine {
	t.Helper()
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ps":
			fmt.Fprint(w, `{"models":[]}`)
		case "/api/chat":
			if started != nil {
				started <- struct{}{}
			}
			if gate != nil {
				<-gate
			}
			fmt.Fprintf(w, "{\"message\":{\"content\":%q},\"done\":true}\n", reply)
		}
	}))
	t.Cleanup(ollama.Close)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lock := &install.Lock{Units: map[string]install.Unit{}, Plan: &selector.Plan{
		Budgets: []selector.Budget{{Pool: "host", BudgetGB: 100}},
		Picks: []selector.Pick{{Role: catalog.RoleBrain, Model: "tiny", Pool: "host", MemGB: 0.01,
			Source: catalog.Source{Kind: catalog.SourceOllama, Runtime: catalog.RuntimeOllama, Tag: "tiny:1b"}}}}}
	cfg := config.Default()
	cfg.Mode, cfg.OllamaURL = config.ModeLocal, ollama.URL
	e, err := engine.New(engine.Options{Config: cfg, Store: st, Lock: lock})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func answerOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %v", resp.StatusCode, out)
	}
	return out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"].(string)
}

const hello = `{"messages":[{"role":"user","content":"hello"}]}`

// A request that read the old engine just as it was swapped and retired
// runs on the new one: it never reaches an engine that is closing.
func TestARequestDuringASwapRunsOnALiveEngine(t *testing.T) {
	a, b := replyEngine(t, "from a", nil, nil), replyEngine(t, "from b", nil, nil)
	s := &Server{Eng: a}
	swapped := false
	s.beforeEnter = func(e *engine.Engine) {
		if e == a && !swapped {
			swapped = true
			s.SetEngine(b).Retire(nil) // nothing holds a yet, so it closes now
		}
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	if got := answerOf(t, post(t, srv, "/v1/chat/completions", hello, "")); got != "from b" {
		t.Fatalf("answered %q, want the new engine's answer", got)
	}
	if a.Enter() {
		t.Fatal("the retired engine still gives holds")
	}
}

// A request already holding the old engine finishes there; the old engine
// closes only after it, however long it takes.
func TestARetiredEngineClosesOnlyAfterItsRequests(t *testing.T) {
	started, gate := make(chan struct{}, 1), make(chan struct{})
	a, b := replyEngine(t, "from a", started, gate), replyEngine(t, "from b", nil, nil)
	s := &Server{Eng: a}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	got := make(chan string, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(hello))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			got <- err.Error()
			return
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		got <- fmt.Sprint(out["choices"])
	}()
	<-started // the request holds a and is being answered
	retired := make(chan struct{})
	go func() { s.SetEngine(b).Retire(nil); close(retired) }()
	if ans := answerOf(t, post(t, srv, "/v1/chat/completions", hello, "")); ans != "from b" {
		t.Fatalf("a new request went to %q", ans)
	}
	select {
	case <-retired:
		t.Fatal("the old engine closed while a request was still using it")
	default:
	}
	close(gate)
	if ans := <-got; !strings.Contains(ans, "from a") {
		t.Fatalf("the request on the old engine was cut: %s", ans)
	}
	<-retired
}

// A brain whose answer stream is cut gives the client an error, never a
// finish_reason of "stop" on half an answer.
func TestACutAnswerIsNeverFinished(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/chat" {
			fmt.Fprintln(w, `{"message":{"content":"The total is 1"},"done":false}`)
		}
	}))
	defer ollama.Close()
	st, _ := store.Open(t.TempDir())
	lock := &install.Lock{Units: map[string]install.Unit{}, Plan: &selector.Plan{
		Budgets: []selector.Budget{{Pool: "host", BudgetGB: 100}},
		Picks: []selector.Pick{{Role: catalog.RoleBrain, Model: "tiny", Pool: "host", MemGB: 0.01,
			Source: catalog.Source{Kind: catalog.SourceOllama, Runtime: catalog.RuntimeOllama, Tag: "tiny:1b"}}}}}
	cfg := config.Default()
	cfg.Mode, cfg.OllamaURL = config.ModeLocal, ollama.URL
	e, err := engine.New(engine.Options{Config: cfg, Store: st, Lock: lock})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&Server{Eng: e}).Handler())
	defer srv.Close()
	resp := post(t, srv, "/v1/chat/completions", hello, "")
	resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Fatal("a cut answer came back as a 200")
	}
	resp = post(t, srv, "/v1/chat/completions", `{"stream":true,"messages":[{"role":"user","content":"hello"}]}`, "")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), `"finish_reason":"stop"`) || !strings.Contains(string(body), `"error"`) {
		t.Fatalf("stream: %s", body)
	}
}

// While the updater's canary holds the server, a new work request waits and
// then runs; reading status does not wait.
func TestAHoldMakesWorkWait(t *testing.T) {
	s := &Server{Eng: replyEngine(t, "after the hold", nil, nil)}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	release := s.Hold()
	got := make(chan string, 1)
	go func() { got <- answerOf(t, post(t, srv, "/v1/chat/completions", hello, "")) }()
	st, err := http.Get(srv.URL + "/v1/brainiac/status")
	if err != nil || st.StatusCode != 200 {
		t.Fatalf("status during a hold: %v %v", st, err)
	}
	st.Body.Close()
	select {
	case a := <-got:
		t.Fatalf("a work request ran during the hold: %q", a)
	case <-time.After(300 * time.Millisecond):
	}
	release()
	if a := <-got; a != "after the hold" {
		t.Fatalf("after the hold: %q", a)
	}
}

// Work requests beyond the slots wait their turn; an outcome body is bounded.
func TestWorkWaitsForASlotAndOutcomesAreBounded(t *testing.T) {
	started, gate := make(chan struct{}, 2), make(chan struct{})
	s := &Server{Eng: replyEngine(t, "ok", started, gate), WorkSlots: 1}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	got := make(chan string, 2)
	for i := 0; i < 2; i++ {
		go func() { got <- answerOf(t, post(t, srv, "/v1/chat/completions", hello, "")) }()
	}
	<-started
	select {
	case <-started:
		t.Fatal("two work requests ran with one slot")
	case <-time.After(300 * time.Millisecond):
	}
	close(gate)
	<-got
	<-got
	big := `{"ok":false,"trace":"` + strings.Repeat("x", 100<<10) + `"}`
	resp := post(t, srv, "/v1/brainiac/outcomes", big, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a 100 KB outcome body: %d", resp.StatusCode)
	}
}

// A fetched page cut to max_chars never ends in half a character.
func TestFetchCutsAtACharacter(t *testing.T) {
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "ab€€€")
	}))
	defer page.Close()
	e := replyEngine(t, "x", nil, nil)
	e.Web().AllowPrivate = true
	e.Web().HTTP = nil
	srv := httptest.NewServer((&Server{Eng: e}).Handler())
	defer srv.Close()
	resp := post(t, srv, "/v1/brainiac/fetch", `{"url":"`+page.URL+`","max_chars":4}`, "")
	defer resp.Body.Close()
	var out struct {
		Text string `json:"text"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Text != "ab" {
		t.Fatalf("cut to %q", out.Text)
	}
}

// The router can be asked where a text would go, without running a model.
func TestRouteAnswersWithoutAModel(t *testing.T) {
	started := make(chan struct{}, 1)
	srv := httptest.NewServer((&Server{Eng: replyEngine(t, "x", started, nil)}).Handler())
	defer srv.Close()
	resp := post(t, srv, "/v1/brainiac/route", `{"text":"Solve 5x + 7 = 2x - 8"}`, "")
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if _, ok := out["routes"]; resp.StatusCode != 200 || !ok {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	select {
	case <-started:
		t.Fatal("asking the router ran a model")
	default:
	}
}
