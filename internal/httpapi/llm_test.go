package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/llm"
)

// fakeClaudeAPI is a Messages API that answers every call with "OK".
func fakeClaudeAPI(t *testing.T) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "sk-ant-good" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"claude-opus-5-5",
			"content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":1}}`))
	}))
	t.Cleanup(srv.Close)

	return srv
}

func llmTestServer(t *testing.T) *Server {
	t.Helper()

	hub, err := llm.NewHub(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	hub.ClaudeBaseURL = fakeClaudeAPI(t).URL

	return &Server{LLM: hub}
}

func TestNewDesktopsRunTheBotWithTheBrainTheyAskFor(t *testing.T) {
	s := llmTestServer(t)

	if b, err := s.desktopBrain("", ""); err != nil || b.Kind != desktop.BrainOllama || b.Hosted() {
		t.Fatalf("default brain = %+v, %v", b, err)
	}

	for _, harness := range []string{"ollama", "claude", "azure"} {
		if _, err := s.desktopBrain(harness, ""); err == nil {
			t.Errorf("harness %q was accepted; every new desktop runs TheBot", harness)
		}
	}

	if _, err := s.desktopBrain("thebot", "claude"); err == nil || !strings.Contains(err.Error(), "not set up") {
		t.Fatalf("a Claude brain nobody set up was accepted: %v", err)
	}

	if err := s.LLM.Save(llm.ProviderClaude, llm.Settings{Key: "sk-ant-good", Model: "claude-sonnet-5-5"}); err != nil {
		t.Fatal(err)
	}

	b, err := s.desktopBrain("", "Claude")
	if err != nil || b.Kind != desktop.BrainClaude || b.Model != "claude-sonnet-5-5" || !b.Vision {
		t.Fatalf("claude brain = %+v, %v", b, err)
	}

	if _, err := s.desktopBrain("", "gpt"); err == nil {
		t.Fatal("an unknown brain was accepted")
	}

	// Only the VM needs nothing set up.
	if b, err := s.desktopBrain("", "none"); err != nil || !b.VMOnly() || b.Hosted() {
		t.Fatalf("VM-only brain = %+v, %v", b, err)
	}
}

func TestSettingsAreSavedOnlyWhenTheModelAnswers(t *testing.T) {
	s := llmTestServer(t)

	rec := postForm(t, s.llmSettingsHandler, "/api/llm/settings", url.Values{"provider": {"claude"}, "key": {"sk-ant-bad"}}.Encode())
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid x-api-key") {
		t.Fatalf("bad key: %d %s", rec.Code, rec.Body)
	}
	if s.LLM.Configured(llm.ProviderClaude) {
		t.Fatal("a key the API refused was saved")
	}

	rec = postForm(t, s.llmSettingsHandler, "/api/llm/settings", url.Values{"provider": {"claude"}, "key": {"sk-ant-good"}}.Encode())
	if rec.Code != http.StatusOK || !s.LLM.Configured(llm.ProviderClaude) {
		t.Fatalf("good key: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "sk-ant") {
		t.Fatal("the settings answer shows the key")
	}
}

func TestTheProxyAnswersTheBotInTheOpenAIShape(t *testing.T) {
	s := llmTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/llm/claude/v1/chat/completions",
		strings.NewReader(`{"model":"default","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	s.llmProxyHandler(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured: %d %s", rec.Code, rec.Body)
	}

	if err := s.LLM.Save(llm.ProviderClaude, llm.Settings{Key: "sk-ant-good"}); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/api/llm/claude/v1/chat/completions", "/api/llm/claude/chat"} {
		req = httptest.NewRequest(http.MethodPost, path,
			strings.NewReader(`{"model":"default","messages":[{"role":"user","content":"hi"}]}`))
		rec = httptest.NewRecorder()
		s.llmProxyHandler(rec, req)
		body, _ := io.ReadAll(rec.Body)
		if rec.Code != http.StatusOK || !strings.Contains(string(body), `"content":"OK"`) {
			t.Fatalf("%s: %d %s", path, rec.Code, body)
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/api/llm/claude/v1/models", nil)
	rec = httptest.NewRecorder()
	s.llmProxyHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("models: %d", rec.Code)
	}

	// The old route is Azure's, which nobody set up here.
	req = httptest.NewRequest(http.MethodPost, "/api/llm/chat", strings.NewReader(`{}`))
	rec = httptest.NewRecorder()
	s.llmProxyHandler(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("legacy azure route: %d", rec.Code)
	}
}
