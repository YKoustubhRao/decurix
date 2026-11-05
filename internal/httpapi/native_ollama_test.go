package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/ollama"
)

func TestNativeOllamaProxy(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/chat" {
			t.Errorf("path=%s", r.URL.Path)
		}
		var got map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("body: %v", err)
		}
		if got["keep_alive"] != ollama.KeepAlive() || got["options"].(map[string]any)["num_ctx"] != float64(ollama.NumCtx) {
			t.Errorf("the model was asked %v", got)
		}
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Error("forwarded dashboard credentials")
		}
		fmt.Fprint(w, "{\"done\":true}\n")
	}))
	defer upstream.Close()
	t.Setenv("DECURIX_WINDOWS_OLLAMA_URL", upstream.URL)
	s := &Server{}
	local := http.NewServeMux()
	s.RegisterLocalRoutes(local)
	req := httptest.NewRequest(http.MethodPost, "/api/native-ollama/api/chat", strings.NewReader(`{"model":"m","keep_alive":0,"options":{"num_ctx":4096}}`))
	req.Header.Set("Cookie", "session=private")
	req.Header.Set("Authorization", "Bearer private")
	res := httptest.NewRecorder()
	local.ServeHTTP(res, req)
	if res.Code != 200 || res.Body.String() != "{\"done\":true}\n" {
		t.Fatalf("response=%d %s", res.Code, res.Body.String())
	}
	for _, route := range []string{"/api/pull", "/api/delete", "/api/create"} {
		res = httptest.NewRecorder()
		local.ServeHTTP(res, httptest.NewRequest(http.MethodPost, "/api/native-ollama"+route, nil))
		if res.Code != 404 {
			t.Fatalf("model management %s returned %d", route, res.Code)
		}
	}
	if calls != 1 {
		t.Fatalf("upstream calls=%d", calls)
	}
	public := http.NewServeMux()
	s.RegisterRoutes(public)
	// The nonlocal mux falls through to the dashboard 404 for non-root paths.
	res = httptest.NewRecorder()
	public.ServeHTTP(res, req)
	if res.Code == 200 {
		t.Fatal("native inference exposed on nonlocal mux")
	}
	if calls != 1 {
		t.Fatal("nonlocal listener forwarded inference")
	}
}

// The proxy forwards to whatever the desktops' model server is - the host's own Ollama, the bundled container, an
// OpenAI-compatible server without its /v1 - or the Windows app's native one (review NET-R3 R2-B1).
func TestTheProxyForwardsToTheDesktopsModelServer(t *testing.T) {
	for _, tc := range []struct{ ollamaURL, windows, want string }{
		{"", "", "http://host.docker.internal:11434"},
		{"http://host.docker.internal:11434", "", "http://host.docker.internal:11434"},
		{ollama.BundledURL, "", ollama.BundledURL},
		{"http://llm.lan:8000/v1/", "", "http://llm.lan:8000"},
		{"http://host.docker.internal:11434", "http://172.20.0.1:11434", "http://172.20.0.1:11434"},
	} {
		t.Setenv("OLLAMA_URL", tc.ollamaURL)
		t.Setenv("DECURIX_WINDOWS_OLLAMA_URL", tc.windows)
		if got := modelProxyTarget(); got != tc.want {
			t.Errorf("OLLAMA_URL=%q windows=%q: the proxy forwards to %q, want %q", tc.ollamaURL, tc.windows, got, tc.want)
		}
	}
}

// No desktop can unload the shared model or load it at another context size: keep_alive and num_ctx are set by the
// proxy - the ordinary size and hold, or exactly the one large size with a short hold for a request that asks more
// context - and an embedding keeps its model's own context (CLAUDE.md: num_ctx fixed at 8192).
func TestTheProxyKeepsTheSharedModelLoaded(t *testing.T) {
	t.Setenv("DECURIX_MODEL_MAX_CTX", "")
	for _, tc := range []struct {
		route, body string
		ctx         any // nil: no num_ctx
		keep        string
	}{
		{"/api/chat", `{"model":"m","messages":[]}`, float64(ollama.NumCtx), ollama.KeepAlive()},
		{"/api/chat", `{"model":"m","keep_alive":0}`, float64(ollama.NumCtx), ollama.KeepAlive()},
		{"/api/generate", `{"model":"m","keep_alive":-1,"options":{"num_ctx":2048,"temperature":0.2}}`, float64(ollama.NumCtx), ollama.KeepAlive()},
		{"/api/chat", `{"model":"m","options":{"num_ctx":32768}}`, float64(32768), largeKeepAlive},
		{"/api/chat", `{"model":"m","options":{"num_ctx":131072}}`, float64(32768), largeKeepAlive},
		{"/api/embed", `{"model":"e","input":"x","keep_alive":0,"options":{"num_ctx":512}}`, nil, ollama.KeepAlive()},
	} {
		out, ok := modelRequestPolicy([]byte(tc.body), tc.route)
		if !ok {
			t.Fatalf("%s %s: refused", tc.route, tc.body)
		}
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		opts, _ := got["options"].(map[string]any)
		if got["keep_alive"] != tc.keep || (tc.ctx == nil && opts != nil && opts["num_ctx"] != nil) || (tc.ctx != nil && opts["num_ctx"] != tc.ctx) {
			t.Errorf("%s %s became %s", tc.route, tc.body, out)
		}
	}
	if _, ok := modelRequestPolicy([]byte(`[1,2]`), "/api/chat"); ok {
		t.Error("a body that is not an object was passed")
	}
	// What else the request says is kept as it was: the prompt's bytes, a temperature.
	out, _ := modelRequestPolicy([]byte(`{"model":"m","prompt":"<b>a & b</b>","options":{"temperature":0.2}}`), "/api/generate")
	if !strings.Contains(string(out), `"prompt":"<b>a & b</b>"`) || !strings.Contains(string(out), `"temperature":0.2`) {
		t.Errorf("the request's own fields changed: %s", out)
	}
}
