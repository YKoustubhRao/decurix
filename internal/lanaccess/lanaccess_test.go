package lanaccess

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func protectedHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("protected: " + r.URL.Path))
	})
}

// login POSTs code to the gated server and returns the response along with
// any Set-Cookie header it issued.
func login(t *testing.T, srv *httptest.Server, code, next string) *http.Response {
	t.Helper()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	resp, err := client.PostForm(srv.URL+loginPath, map[string][]string{
		"code": {code},
		"next": {next},
	})
	if err != nil {
		t.Fatal(err)
	}

	return resp
}

func TestUnauthenticatedRequestGetsTheGatePage(t *testing.T) {
	store := NewStore()
	srv := httptest.NewServer(store.Middleware(protectedHandler()))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/view/research-box")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)

	if !strings.Contains(string(body[:n]), "Enter the access code") {
		t.Fatalf("expected the gate page, got: %s", body[:n])
	}
}

func TestCorrectCodeGrantsASessionAndRedirectsOn(t *testing.T) {
	store := NewStore()
	srv := httptest.NewServer(store.Middleware(protectedHandler()))
	defer srv.Close()

	resp := login(t, srv, store.Password(), "/view/research-box")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}

	if loc := resp.Header.Get("Location"); loc != "/view/research-box" {
		t.Fatalf("redirect location = %q, want /view/research-box", loc)
	}

	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "flowera_lan_session" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie was set")
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/view/research-box", nil)
	req.AddCookie(cookie)

	authed, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer authed.Body.Close()

	body := make([]byte, 4096)
	n, _ := authed.Body.Read(body)

	if !strings.HasPrefix(string(body[:n]), "protected: /view/research-box") {
		t.Fatalf("expected the real handler to run once authenticated, got: %s", body[:n])
	}
}

func TestWrongCodeDoesNotGrantASession(t *testing.T) {
	store := NewStore()
	srv := httptest.NewServer(store.Middleware(protectedHandler()))
	defer srv.Close()

	resp := login(t, srv, "not-the-code", "/")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (re-shown gate)", resp.StatusCode)
	}

	for _, c := range resp.Cookies() {
		if c.Name == "flowera_lan_session" {
			t.Fatal("a wrong code must not set a session cookie")
		}
	}
}

func TestLockoutAfterRepeatedFailures(t *testing.T) {
	store := NewStore()
	srv := httptest.NewServer(store.Middleware(protectedHandler()))
	defer srv.Close()

	var last *http.Response
	for i := 0; i < maxAttempts; i++ {
		last = login(t, srv, "wrong", "/")
		last.Body.Close()
	}

	if last.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status after %d failures = %d, want 429", maxAttempts, last.StatusCode)
	}

	// Even the correct code is refused while locked out.
	resp := login(t, srv, store.Password(), "/")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status for correct code while locked out = %d, want 429", resp.StatusCode)
	}

	for _, c := range resp.Cookies() {
		if c.Name == "flowera_lan_session" {
			t.Fatal("a locked-out request must not grant a session even with the right code")
		}
	}
}

func TestRotateDropsExistingSessions(t *testing.T) {
	store := NewStore()
	srv := httptest.NewServer(store.Middleware(protectedHandler()))
	defer srv.Close()

	resp := login(t, srv, store.Password(), "/")
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "flowera_lan_session" {
			cookie = c
		}
	}
	resp.Body.Close()
	if cookie == nil {
		t.Fatal("no session cookie was set")
	}

	store.Rotate()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/view/research-box", nil)
	req.AddCookie(cookie)

	after, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer after.Body.Close()

	body := make([]byte, 4096)
	n, _ := after.Body.Read(body)

	if strings.HasPrefix(string(body[:n]), "protected:") {
		t.Fatal("a session from before Rotate must no longer be valid")
	}
}

func TestSanitizeNextRejectsProtocolRelativeURLs(t *testing.T) {
	cases := map[string]string{
		"":                         "/",
		"/view/research-box":       "/view/research-box",
		"//evil.example.com":       "/",
		"https://evil.example.com": "/",
		"relative/path":            "/",
	}

	for input, want := range cases {
		if got := sanitizeNext(input); got != want {
			t.Errorf("sanitizeNext(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestPasswordIsStableUntilRotated(t *testing.T) {
	store := NewStore()

	first := store.Password()
	second := store.Password()

	if first != second {
		t.Fatalf("Password() changed without a Rotate: %q vs %q", first, second)
	}

	if len(first) != passwordLen {
		t.Fatalf("password length = %d, want %d", len(first), passwordLen)
	}

	rotated := store.Rotate()
	if rotated == first {
		t.Fatal("Rotate() returned the same code")
	}
}
