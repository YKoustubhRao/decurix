package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"thebot/internal/llm"
	"thebot/internal/trace"
)

// callProxy stands in for the control plane's model proxy behind soul's OpenAI provider: it keeps each call's headers
// and body and answers with answer (or status and its error message).
type callProxy struct {
	mu      sync.Mutex
	headers []http.Header
	bodies  []map[string]any
	status  int
	message string
}

func (p *callProxy) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		p.mu.Lock()
		p.headers = append(p.headers, r.Header.Clone())
		p.bodies = append(p.bodies, body)
		status, message := p.status, p.message
		p.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": message}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "{\"ok\": true}"}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 120, "completion_tokens": 7, "prompt_tokens_details": map[string]any{"cached_tokens": 100}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// callServer is a soul with the OpenAI provider pointed at a callProxy, a trace writer and a call key.
func callServer(t *testing.T) (*server, *callProxy, string) {
	t.Helper()
	proxy := &callProxy{}
	s := newProxiedServer(t, proxy.start(t))
	dir := filepath.Join(t.TempDir(), "traces")
	s.tracer = trace.New(dir)
	s.callKey = "call-key-for-tests"
	return s, proxy, dir
}

// postCall sends body to /api/llm/call with bearer as the Authorization.
func postCall(s *server, bearer, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	s.mux().ServeHTTP(rec, req)
	return rec
}

func callBody(t *testing.T, fields map[string]any) string {
	t.Helper()
	body := map[string]any{"brain": "hosted", "purpose": "py.judge", "task": "t0123456789abcdef", "turn": "j42:7",
		"messages":   []map[string]any{{"role": "system", "content": "You judge."}, {"role": "user", "content": "Is it done?"}},
		"max_tokens": 1500}
	for k, v := range fields {
		if v == nil {
			delete(body, k)
			continue
		}
		body[k] = v
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func answerOf(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("not JSON: %s", rec.Body.String())
	}
	return out
}

func TestLLMCallNeedsTheAgentTokenOrTheCallKey(t *testing.T) {
	s, proxy, _ := callServer(t)
	body := callBody(t, nil)
	for _, bearer := range []string{"", "wrong", "call-key-for-tests-not"} {
		if rec := postCall(s, bearer, llmCallPath, body); rec.Code != http.StatusUnauthorized {
			t.Fatalf("bearer %q: got %d, want 401", bearer, rec.Code)
		}
	}
	for _, bearer := range []string{testAgentToken, "call-key-for-tests"} {
		if rec := postCall(s, bearer, llmCallPath, body); rec.Code != http.StatusOK {
			t.Fatalf("bearer %q: got %d (%s)", bearer, rec.Code, rec.Body.String())
		}
	}
	if len(proxy.bodies) != 2 {
		t.Fatalf("the proxy saw %d calls, want 2", len(proxy.bodies))
	}
	// The call key opens this route and no other: never a job, a reply or the memory.
	for _, path := range []string{"/api/chat", "/api/memory", "/api/jobs/x"} {
		if rec := postCall(s, "call-key-for-tests", path, `{"prompt":"rm -rf ~"}`); rec.Code != http.StatusUnauthorized {
			t.Fatalf("the call key opened %s: %d", path, rec.Code)
		}
	}
	// A soul with no agent token fails closed, call key or not.
	s.cfg.AgentToken = ""
	if rec := postCall(s, "call-key-for-tests", llmCallPath, body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a soul with no agent token answered %d", rec.Code)
	}
}

func TestLLMCallGoesOutAsSoulsOwnCallsDoWithTheCallersPurpose(t *testing.T) {
	s, proxy, dir := callServer(t)
	rec := postCall(s, testAgentToken, llmCallPath, callBody(t, map[string]any{
		"response_format": map[string]any{"type": "json_object"}}))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	out := answerOf(t, rec)
	if out["text"] != `{"ok": true}` || out["truncated"] != false {
		t.Fatalf("answer: %v", out)
	}
	usage, _ := out["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(120) || usage["completion_tokens"] != float64(7) || usage["cached_tokens"] != float64(100) {
		t.Fatalf("usage: %v", usage)
	}

	h := proxy.headers[0]
	if h.Get(llm.HeaderPurpose) != "py.judge" || h.Get(llm.HeaderTask) != "t0123456789abcdef" ||
		h.Get(llm.HeaderTurn) != "j42:7" || h.Get(llm.HeaderCaller) != llm.Caller {
		t.Fatalf("the proxy's tags: %v", h)
	}
	body := proxy.bodies[0]
	if body["prompt_cache_key"] == nil || body["max_tokens"] != float64(1500) {
		t.Fatalf("the body is not soul's own shape: %v", body)
	}
	if rf, _ := body["response_format"].(map[string]any); rf["type"] != "json_object" {
		t.Fatalf("response_format not passed on: %v", body["response_format"])
	}
	if strings.Contains(mustJSON(body), "py.judge") {
		t.Fatal("the purpose reached the request body: the prompt prefix would change with it")
	}

	raw, err := os.ReadFile(filepath.Join(dir, "t0123456789abcdef.jsonl"))
	if err != nil {
		t.Fatalf("no trace line: %v", err)
	}
	var line trace.Line
	if err := json.Unmarshal(raw, &line); err != nil {
		t.Fatalf("trace line: %v (%s)", err, raw)
	}
	if line.Kind != "call" || line.Purpose != "py.judge" || line.Job != "j42" || line.Turn != "j42:7" ||
		line.Usage == nil || line.Usage.In != 120 || line.Usage.Out != 7 || line.Messages != 2 {
		t.Fatalf("trace line: %+v", line)
	}
	// Not a job: the call is counted once, by the agent from the usage above, and in no job of soul's.
	if len(s.jobs) != 0 {
		t.Fatalf("a call made a job: %v", s.jobs)
	}
}

func TestLLMCallWithholdsTheTasksSecrets(t *testing.T) {
	s, proxy, dir := callServer(t)
	const secret = "hunter2-Pa55word"
	proxy.status, proxy.message = http.StatusBadRequest, "invalid request near '"+secret+"'"
	rec := postCall(s, testAgentToken, llmCallPath, callBody(t, map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "Log in with " + secret}},
		"secrets":  []string{secret}}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	out := answerOf(t, rec)
	if out["where"] != "provider" || strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("the provider's error came back with the secret, or not as the provider's: %s", rec.Body.String())
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "t0123456789abcdef.jsonl"))
	if len(raw) == 0 || strings.Contains(string(raw), secret) {
		t.Fatalf("the trace holds the secret or nothing: %s", raw)
	}
	var line trace.Line
	_ = json.Unmarshal(raw, &line)
	if !line.Failed || line.Purpose != "py.judge" {
		t.Fatalf("a failed call's line: %+v", line)
	}
}

func TestLLMCallRefusalsAreSoulsAndBounded(t *testing.T) {
	s, proxy, _ := callServer(t)
	many := make([]map[string]any, maxLLMCallMessages+1)
	for i := range many {
		many[i] = map[string]any{"role": "user", "content": "x"}
	}
	for name, fields := range map[string]map[string]any{
		"soul's own purpose": {"purpose": llm.PurposeOperatorTurn},
		"no purpose":         {"purpose": nil},
		"another brain":      {"brain": "local"},
		"no messages":        {"messages": []any{}},
		"too many messages":  {"messages": many},
		"too many tokens":    {"max_tokens": maxLLMCallTokens + 1},
		"a stranger role":    {"messages": []map[string]any{{"role": "tool", "content": "x"}}},
		"a remote picture": {"messages": []map[string]any{{"role": "user", "content": []map[string]any{
			{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/a.png"}}}}}},
		"a strange format": {"response_format": map[string]any{"type": "grammar"}},
		"too many secrets": {"secrets": make([]string, 1001)},
	} {
		rec := postCall(s, testAgentToken, llmCallPath, callBody(t, fields))
		if rec.Code != http.StatusBadRequest || answerOf(t, rec)["where"] != "soul" {
			t.Errorf("%s: got %d %s, want 400 from soul", name, rec.Code, rec.Body.String())
		}
	}
	big := `{"purpose":"py.judge","messages":[{"role":"user","content":"` + strings.Repeat("a", maxLLMCallBody) + `"}]}`
	if rec := postCall(s, testAgentToken, llmCallPath, big); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("a call past %d bytes: got %d", maxLLMCallBody, rec.Code)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, llmCallPath, nil)
	req.Header.Set("Authorization", "Bearer "+testAgentToken)
	s.mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: got %d", rec.Code)
	}
	if len(proxy.bodies) != 0 {
		t.Fatalf("a refused call reached the proxy %d times", len(proxy.bodies))
	}
}

func TestLLMCallPassesTheProvidersStatusOn(t *testing.T) {
	s, proxy, _ := callServer(t)
	proxy.status, proxy.message = http.StatusNotFound, "no such deployment"
	rec := postCall(s, testAgentToken, llmCallPath, callBody(t, nil))
	if rec.Code != http.StatusNotFound || answerOf(t, rec)["where"] != "provider" {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

func TestMessagesOfKeepsTextAndPicturesInOrder(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...)
	url := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	content, _ := json.Marshal([]map[string]any{
		{"type": "text", "text": "Judge the work."},
		{"type": "text", "text": "A page of a document:"},
		{"type": "image_url", "image_url": map[string]any{"url": url}},
		{"type": "text", "text": "The screen NOW:"},
		{"type": "image_url", "image_url": map[string]any{"url": url}},
		{"type": "text", "text": "Answer in JSON."},
	})
	got, err := messagesOf(callMessage{Role: "user", Content: content})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want the message and its trailing text: %+v", got)
	}
	m := got[0]
	if m.Content != "Judge the work.\nA page of a document:" || len(m.Images) != 2 || string(m.Images[0]) != string(png) {
		t.Fatalf("message: %q, %d pictures", m.Content, len(m.Images))
	}
	if m.ImageLabel(0) != "" || m.ImageLabel(1) != "The screen NOW:" {
		t.Fatalf("labels: %q", m.ImageLabels)
	}
	if got[1].Role != "user" || got[1].Content != "Answer in JSON." {
		t.Fatalf("trailing text: %+v", got[1])
	}

	plain, _ := json.Marshal("hello")
	if got, err := messagesOf(callMessage{Role: "system", Content: plain}); err != nil || len(got) != 1 || got[0].Content != "hello" {
		t.Fatalf("a string message: %+v %v", got, err)
	}
}

func TestTheCallKeyFileIsTheWorkersOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "decurix", "soul-call.key")
	if err := writeCallKey(path, "k1"); err != nil {
		t.Fatal(err)
	}
	if err := writeCallKey(path, "k2"); err != nil { // replaced whole on the next start
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(raw)) != "k2" {
		t.Fatalf("key file: %q %v", raw, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Fatalf("key file mode %v: readable by others", info.Mode().Perm())
	}
	if got := callKeyPath("/home/headless/.decurix/ai"); filepath.ToSlash(got) != "/home/headless/.decurix/soul-call.key" {
		t.Fatalf("callKeyPath = %s", got)
	}
	if a, b := newCallKey(), newCallKey(); a == b || len(a) != 64 {
		t.Fatalf("call keys: %q %q", a, b)
	}
}
