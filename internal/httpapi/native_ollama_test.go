package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
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
