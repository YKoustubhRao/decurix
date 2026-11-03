package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
