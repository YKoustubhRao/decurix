package websearch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The real bug this pins: sec.gov 403s EVERY request carrying a
// browser-style User-Agent with "Your Request Originates from an
// Undeclared Automated Tool", and only serves a client that declares
// itself, per its own published access policy. FetchPageText must retry
// once with a declared identity on exactly a 403 - not on other errors,
// and not more than once.
func TestFetchPageTextRetriesWithDeclaredUAOn403(t *testing.T) {
	var uas []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua := r.Header.Get("User-Agent")
		uas = append(uas, ua)
		if ua == userAgent {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte("blocked"))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("<html><body>real content</body></html>"))
	}))
	defer srv.Close()

	text, err := FetchPageText(context.Background(), srv.URL, 10000)
	if err != nil {
		t.Fatalf("FetchPageText() error = %v, want it to recover via the declared-UA retry", err)
	}
	if !strings.Contains(text, "real content") {
		t.Errorf("text = %q, want the real page content", text)
	}
	if len(uas) != 2 {
		t.Fatalf("server saw %d request(s), want exactly 2 (browser attempt, then declared retry)", len(uas))
	}
	if uas[0] != userAgent {
		t.Errorf("first request UA = %q, want the browser UA tried first", uas[0])
	}
	if uas[1] != declaredUserAgent {
		t.Errorf("retry UA = %q, want the declared identity", uas[1])
	}
}

// A non-403 failure (404, 500, timeout) must not trigger the retry - that
// would just be a second pointless request against a page that's simply
// missing or broken.
func TestFetchPageTextDoesNotRetryOnNon403(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := FetchPageText(context.Background(), srv.URL, 10000)
	if err == nil {
		t.Fatal("FetchPageText() error = nil for a 404, want an error")
	}
	if requests != 1 {
		t.Errorf("server saw %d request(s), want exactly 1 - no retry on a non-403", requests)
	}
}

// A page that 403s both attempts must report a real, honest failure -
// never silently succeed with empty content.
func TestFetchPageTextFailsHonestlyWhenBothUAsAreBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	_, err := FetchPageText(context.Background(), srv.URL, 10000)
	if err == nil {
		t.Fatal("FetchPageText() error = nil when both UAs are blocked, want a real error")
	}
}
