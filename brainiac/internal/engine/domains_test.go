package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"brainiac/internal/catalog"
	"brainiac/internal/config"
	"brainiac/internal/evals"
	"brainiac/internal/selector"
)

// taggedOllama answers each model tag with its own name, so a test can see who answered.
type taggedOllama struct {
	mu   sync.Mutex
	seen []string
}

func (f *taggedOllama) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/ps":
		fmt.Fprint(w, `{"models":[]}`)
	case "/api/chat":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		tag, _ := body["model"].(string)
		f.mu.Lock()
		f.seen = append(f.seen, tag)
		f.mu.Unlock()
		fmt.Fprintf(w, "{\"message\":{\"content\":\"answer from %s\"},\"done\":true,\"prompt_eval_count\":1,\"eval_count\":1}\n", tag)
	default:
		http.NotFound(w, r)
	}
}

func domainEngine(t *testing.T, mode string, remote *httptest.Server) (*Engine, *taggedOllama) {
	t.Helper()
	fo := &taggedOllama{}
	srv := httptest.NewServer(fo)
	t.Cleanup(srv.Close)
	cfg := config.Default()
	cfg.Mode, cfg.OllamaURL = mode, srv.URL
	if remote != nil {
		cfg.Remote = &config.Remote{Kind: config.RemoteDecurix, URL: remote.URL, Provider: "azure", Model: "default"}
	}
	e := newEngine(t, cfg, lockWith(
		ollamaPick(catalog.RoleBrain, "brain-2b", "brain:2b"),
		selector.Pick{Role: "math", Model: "math-1.5b", Pool: "host", MemGB: 0.1,
			Source: catalog.Source{Kind: catalog.SourceOllama, Runtime: catalog.RuntimeOllama, Tag: "math:1.5b"}},
	))
	return e, fo
}

func ask(t *testing.T, e *Engine, model, domain, text string) *Response {
	t.Helper()
	res, err := e.Chat(context.Background(), Request{Model: model, Domain: domain, Messages: []InMessage{{Role: "user", Parts: []Part{{Kind: "text", Text: text}}}}})
	if err != nil {
		t.Fatalf("%s: %v", text, err)
	}
	return res
}

func TestMathGoesToTheMathSpecialistLocally(t *testing.T) {
	e, _ := domainEngine(t, config.ModeLocal, nil)
	res := ask(t, e, "brainiac", "", "Solve 5x + 7 = 2x - 8")
	if res.Brain != "math-1.5b" || res.Trace.Domain != "math" {
		t.Fatalf("brain %s domain %s notes %v", res.Brain, res.Trace.Domain, res.Trace.Notes)
	}
	if res := ask(t, e, "brainiac", "", "write a short poem about the monsoon"); res.Brain != "brain-2b" {
		t.Fatalf("a poem went to %s", res.Brain)
	}
	// A field with no specialist installed stays with the brain, and says so.
	res = ask(t, e, "brainiac", "law", "Is a verbal agreement binding?")
	if res.Brain != "brain-2b" || !strings.Contains(strings.Join(res.Trace.Notes, " "), "no law specialist") {
		t.Fatalf("law: brain %s notes %v", res.Brain, res.Trace.Notes)
	}
	// Naming the field or the field model routes without the router.
	if res := ask(t, e, "brainiac-math", "", "hello"); res.Brain != "math-1.5b" {
		t.Fatalf("brainiac-math went to %s", res.Brain)
	}
	// A concrete model name is that model alone.
	if res := ask(t, e, "brain-2b", "", "Solve 5x + 7 = 2x - 8"); res.Brain != "brain-2b" {
		t.Fatalf("a named model was rerouted to %s", res.Brain)
	}
}

func TestMeasuredWorseSpecialistLosesItsField(t *testing.T) {
	e, _ := domainEngine(t, config.ModeLocal, nil)
	r := &evals.Results{}
	r.Set("math-1.5b", "math", evals.Score{N: 30, Correct: 12})
	r.Set("brain-2b", "math", evals.Score{N: 30, Correct: 20})
	if err := r.Save(e.st.Path("evals.json")); err != nil {
		t.Fatal(err)
	}
	res := ask(t, e, "brainiac", "", "Solve 5x + 7 = 2x - 8")
	if res.Brain != "brain-2b" || !strings.Contains(strings.Join(res.Trace.Notes, " "), "measured worse") {
		t.Fatalf("brain %s notes %v", res.Brain, res.Trace.Notes)
	}
}

func TestWithARemoteBrainTheSpecialistMustEarnTheField(t *testing.T) {
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"model":"gpt-5.4","choices":[{"message":{"content":"remote answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer cp.Close()
	e, _ := domainEngine(t, config.ModeHybrid, cp)
	res := ask(t, e, "brainiac", "", "Solve 5x + 7 = 2x - 8")
	if res.Brain != "decurix:azure" || res.Trace.Domain != "math" {
		t.Fatalf("unmeasured: brain %s domain %s", res.Brain, res.Trace.Domain)
	}
	r := &evals.Results{}
	r.Set("math-1.5b", "math", evals.Score{N: 40, Correct: 36})
	r.Set(RemoteEvalName, "math", evals.Score{N: 40, Correct: 35})
	if err := r.Save(e.st.Path("evals.json")); err != nil {
		t.Fatal(err)
	}
	res = ask(t, e, "brainiac", "", "Solve 5x + 7 = 2x - 8")
	if res.Brain != "math-1.5b" || res.Trace.RemoteT != 0 {
		t.Fatalf("measured at least as good: brain %s remote tokens %d", res.Brain, res.Trace.RemoteT)
	}
	// The remote brain alone stays reachable by name, for evals.
	if res := ask(t, e, RemoteEvalName, "", "Solve 5x + 7 = 2x - 8"); res.Brain != "decurix:azure" {
		t.Fatalf("remote by name went to %s", res.Brain)
	}
}
