package lanaccess

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

// authenticateViaLink GETs authPath with code and next as query parameters -
// the one-click link build.sh/rebuild.sh print - and returns the response
// along with any Set-Cookie header it issued.
func authenticateViaLink(t *testing.T, srv *httptest.Server, code, next string) *http.Response {
	t.Helper()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	resp, err := client.Get(srv.URL + authPath + "?code=" + code + "&next=" + next)
	if err != nil {
		t.Fatal(err)
	}

	return resp
}

// assertSessionGrantedFor checks that a correct code redirected on to /view/research-box with a session cookie
// that then reaches the real handler.
func assertSessionGrantedFor(t *testing.T, srv *httptest.Server, resp *http.Response) {
	t.Helper()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}

	if loc := resp.Header.Get("Location"); loc != "/view/research-box" {
		t.Fatalf("redirect location = %q, want /view/research-box", loc)
	}

	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "decurix_lan_session" {
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

func TestAuthenticateLinkGrantsASessionAndRedirectsOn(t *testing.T) {
	store := NewStore()
	srv := httptest.NewServer(store.Middleware(protectedHandler()))
	defer srv.Close()

	resp := authenticateViaLink(t, srv, store.Password(), "/view/research-box")
	defer resp.Body.Close()

	assertSessionGrantedFor(t, srv, resp)
}

func TestAuthenticateLinkWithWrongCodeDoesNotGrantASession(t *testing.T) {
	store := NewStore()
	srv := httptest.NewServer(store.Middleware(protectedHandler()))
	defer srv.Close()

	resp := authenticateViaLink(t, srv, "not-the-code", "/")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (re-shown gate)", resp.StatusCode)
	}

	for _, c := range resp.Cookies() {
		if c.Name == "decurix_lan_session" {
			t.Fatal("a wrong code must not set a session cookie")
		}
	}
}

func TestAuthenticateLinkWithNoCodeShowsPlainGateWithoutCountingAsAFailure(t *testing.T) {
	store := NewStore()
	srv := httptest.NewServer(store.Middleware(protectedHandler()))
	defer srv.Close()

	// A bare visit to authPath (no ?code= at all) must not count against the
	// lockout - only an actually wrong code should.
	for i := 0; i < maxAttempts; i++ {
		resp := authenticateViaLink(t, srv, "", "/")
		resp.Body.Close()
	}

	resp := authenticateViaLink(t, srv, store.Password(), "/")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status for the correct code after bare visits = %d, want 302 (not locked out)", resp.StatusCode)
	}
}

func TestAuthenticateLinkFailuresCountTowardLockout(t *testing.T) {
	store := NewStore()
	srv := httptest.NewServer(store.Middleware(protectedHandler()))
	defer srv.Close()

	var last *http.Response
	for i := 0; i < maxAttempts; i++ {
		last = authenticateViaLink(t, srv, "wrong", "/")
		last.Body.Close()
	}

	if last.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status after %d failures via link = %d, want 429", maxAttempts, last.StatusCode)
	}
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

	assertSessionGrantedFor(t, srv, resp)
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
		if c.Name == "decurix_lan_session" {
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
		if c.Name == "decurix_lan_session" {
			t.Fatal("a locked-out request must not grant a session even with the right code")
		}
	}
}

func TestStaleAttemptsAreEvicted(t *testing.T) {
	store := NewStore()

	// A source that failed once, long enough ago to be outside both the
	// counting window and any lockout, and never came back.
	store.attemptsMu.Lock()
	store.attempts["stale"] = &attemptState{
		count:       1,
		windowStart: time.Now().Add(-2 * lockoutWindow),
	}
	store.attemptsMu.Unlock()

	// Any later failure (from a different source) should sweep it out.
	store.recordFailure("someone-else")

	store.attemptsMu.Lock()
	_, stillThere := store.attempts["stale"]
	store.attemptsMu.Unlock()

	if stillThere {
		t.Fatal("expected a stale attempt record to be evicted, but it is still present")
	}
}

func TestRotateDropsExistingSessions(t *testing.T) {
	store := NewStore()
	srv := httptest.NewServer(store.Middleware(protectedHandler()))
	defer srv.Close()

	resp := login(t, srv, store.Password(), "/")
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "decurix_lan_session" {
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

func TestRunAutoRotateDisabledByNonPositiveInterval(t *testing.T) {
	store := NewStore()

	done := make(chan struct{})
	go func() {
		store.RunAutoRotate(context.Background(), 0)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunAutoRotate with a non-positive interval should return immediately instead of looping")
	}
}

func TestRunAutoRotateRotatesOnInterval(t *testing.T) {
	store := NewStore()
	first := store.Password()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		store.RunAutoRotate(ctx, 10*time.Millisecond)
		close(done)
	}()

	deadline := time.Now().Add(time.Second)
	for store.Password() == first {
		if time.Now().After(deadline) {
			t.Fatal("RunAutoRotate did not rotate the code within the deadline")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunAutoRotate did not return after its context was cancelled")
	}
}

// A redirect after the access code stays on this site, whatever a browser would make of the path.
func TestSanitizeNextStaysOnThisSite(t *testing.T) {
	for next, want := range map[string]string{
		"/tasks?id=3":      "/tasks?id=3",
		"/desktop/box/":    "/desktop/box/",
		"//evil.com":       "/",
		`/\evil.com`:       "/",
		"/\t/evil.com":     "/",
		"https://evil.com": "/",
		"":                 "/",
		"tasks":            "/",
	} {
		if got := sanitizeNext(next); got != want {
			t.Errorf("sanitizeNext(%q) = %q, want %q", next, got, want)
		}
	}
}

// TestRetiredCodeCostsNoAttempt: a phone retrying the code Rotate replaced is told so and grants nothing, but does not
// lock the source out - behind the tunnel every visitor shares one, so it would lock the person out too.
func TestRetiredCodeCostsNoAttempt(t *testing.T) {
	store := NewStore()
	old := store.Password()
	store.Rotate()
	srv := httptest.NewServer(store.Middleware(protectedHandler()))
	defer srv.Close()

	for i := 0; i < maxAttempts+2; i++ {
		resp := login(t, srv, old, "/")
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("attempt %d with the retired code locked the source out", i+1)
		}
		for _, c := range resp.Cookies() {
			if c.Name == "decurix_lan_session" && c.Value != "" {
				t.Fatal("the retired code granted a session")
			}
		}
	}
	resp := login(t, srv, store.Password(), "/")
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		t.Fatal("the current code was refused after retired-code retries")
	}
}
