// hostguard_test.go checks that a keyless Brainiac answers only this
// machine's programs: a rebinding Host or a cross-site Origin is refused,
// loopback names and allowed names pass, and a key keeps today's rules.
package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// guarded runs one request through auth in front of a handler that says ok.
func guarded(s *Server, method, host, origin, path string) *httptest.ResponseRecorder {
	h := s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	r := httptest.NewRequest(method, "http://placeholder"+path, strings.NewReader(`{}`))
	r.Host = host
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestKeylessRefusesARebindingHost(t *testing.T) {
	s := &Server{}
	for _, host := range []string{"evil.example", "evil.example:8484", "attacker.localhost:8484", "localhost.evil.example",
		"192.168.1.5:8484", "0.0.0.0:8484", "host.docker.internal:8484", "", "[fe80::1]:8484", "localhost.:8484"} {
		for _, path := range []string{"/v1/chat/completions", "/healthz"} {
			w := guarded(s, http.MethodPost, host, "", path)
			if w.Code != http.StatusForbidden {
				t.Fatalf("host %q path %s: status %d, want 403", host, path, w.Code)
			}
			if host != "" && strings.Contains(w.Body.String(), strings.Split(host, ":")[0]) {
				t.Fatalf("the refusal echoed the Host: %s", w.Body.String())
			}
			var out struct {
				Error struct{ Type string } `json:"error"`
			}
			if json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Error.Type != "forbidden_host" {
				t.Fatalf("host %q: body %s", host, w.Body.String())
			}
		}
	}
}

func TestKeylessAllowsLoopbackNames(t *testing.T) {
	s := &Server{}
	for _, host := range []string{"127.0.0.1", "127.0.0.1:8484", "127.1.2.3:8484", "localhost", "LOCALHOST:8484",
		"[::1]:8484", "[::1]", "::1"} {
		if w := guarded(s, http.MethodPost, host, "", "/v1/chat/completions"); w.Code != 200 {
			t.Fatalf("host %q: status %d %s", host, w.Code, w.Body.String())
		}
	}
}

func TestAllowedHostsAreTaken(t *testing.T) {
	s := &Server{AllowedHosts: []string{" Host.Docker.Internal ", "brainiac.lan:9999"}}
	for _, host := range []string{"host.docker.internal:8484", "HOST.DOCKER.INTERNAL", "brainiac.lan:8484"} {
		if w := guarded(s, http.MethodPost, host, "", "/v1/chat/completions"); w.Code != 200 {
			t.Fatalf("allowed host %q: status %d", host, w.Code)
		}
	}
	if w := guarded(s, http.MethodPost, "docker.internal:8484", "", "/v1/chat/completions"); w.Code != http.StatusForbidden {
		t.Fatalf("a name not on the list passed: %d", w.Code)
	}
	// An allowed name is allowed as an Origin too.
	if w := guarded(s, http.MethodPost, "127.0.0.1:8484", "http://brainiac.lan:3000", "/v1/chat/completions"); w.Code != 200 {
		t.Fatalf("allowed origin: status %d", w.Code)
	}
}

func TestKeylessRefusesACrossSiteOrigin(t *testing.T) {
	s := &Server{}
	for _, origin := range []string{"https://evil.example", "http://evil.example:8484", "null", "file://", "chrome-extension://abc",
		"http://127.0.0.1.evil.example"} {
		w := guarded(s, http.MethodPost, "127.0.0.1:8484", origin, "/v1/chat/completions")
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "forbidden_origin") {
			t.Fatalf("origin %q: status %d %s", origin, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "evil") {
			t.Fatalf("the refusal echoed the Origin: %s", w.Body.String())
		}
	}
	for _, origin := range []string{"", "http://localhost:3000", "http://127.0.0.1:8484", "http://[::1]:5173"} {
		if w := guarded(s, http.MethodPost, "localhost:8484", origin, "/v1/chat/completions"); w.Code != 200 {
			t.Fatalf("origin %q: status %d", origin, w.Code)
		}
	}
}

// With a key set, the key is the guard: any Host passes with it, and
// /healthz stays open as before.
func TestKeySetKeepsTodaysRules(t *testing.T) {
	s := &Server{APIKey: "k"}
	if w := guarded(s, http.MethodPost, "brainiac.lan:8484", "", "/v1/chat/completions"); w.Code != http.StatusUnauthorized {
		t.Fatalf("no key: status %d", w.Code)
	}
	h := s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	r := httptest.NewRequest(http.MethodPost, "http://brainiac.lan:8484/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer k")
	r.Header.Set("Origin", "https://app.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("with the key from another name: status %d", w.Code)
	}
	if w := guarded(s, http.MethodGet, "brainiac.lan:8484", "", "/healthz"); w.Code != 200 {
		t.Fatalf("healthz with a key set: status %d", w.Code)
	}
}

// The real handler refuses a rebinding Host before any work runs.
func TestHandlerRefusesARebindingHost(t *testing.T) {
	srv := testServer(t, "")
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	req.Host = "rebind.evil.example:8484"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d", resp.StatusCode)
	}
	// The same request under its own address is answered.
	resp = post(t, srv, "/v1/chat/completions", `{"messages":[{"role":"user","content":"hi"}]}`, "")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("loopback status %d", resp.StatusCode)
	}
}
