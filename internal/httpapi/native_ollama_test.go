package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
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
	req := httptest.NewRequest(http.MethodPost, "/api/native-ollama/api/chat", nil)
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

// The bundled model server is reached through the same allow-listed proxy when it is the desktops' model server, and
// a desktop is given the proxy's address, never the model server's own (review NET-R3 B3).
func TestTheBundledModelIsReachedThroughTheProxy(t *testing.T) {
	t.Setenv("DECURIX_WINDOWS_OLLAMA_URL", "")
	t.Setenv("OLLAMA_URL", ollama.BundledURL)
	if got := modelProxyTarget(); got != ollama.BundledURL {
		t.Fatalf("the proxy forwards to %q, want the bundled %s", got, ollama.BundledURL)
	}
	t.Setenv("OLLAMA_URL", "http://host.docker.internal:11434")
	if got := modelProxyTarget(); got != "" {
		t.Fatalf("with the host's own model server the proxy forwards to %q, want nothing", got)
	}
}
