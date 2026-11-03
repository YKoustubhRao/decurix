package httpapi

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// readAll is a handler that reads its whole body, as a form or JSON read does, and answers how it went.
func readAll(w http.ResponseWriter, r *http.Request) {
	if _, err := io.ReadAll(r.Body); err != nil {
		badJSON(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// Every route has an outer cap on its body, forms included (RW1-E-04); the routes built for more keep their size.
func TestBodyLimitCapsEveryRouteAndKeepsTheBigOnes(t *testing.T) {
	h := BodyLimit(http.HandlerFunc(readAll))
	post := func(path string, n int) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(make([]byte, n))))
		return rec.Code
	}

	for _, path := range []string{"/api/host-bridge/exec", "/api/desktops/create", "/api/lan-access/login", "/anything"} {
		if code := post(path, defaultBodyLimit+1); code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s with %d bytes: %d, want 413", path, defaultBodyLimit+1, code)
		}
		if code := post(path, 1000); code != http.StatusOK {
			t.Errorf("%s with a small body: %d", path, code)
		}
	}
	for path, limit := range bodyLimits {
		probe := path
		if strings.HasSuffix(probe, "/") {
			probe += "x"
		}
		if got := bodyLimitFor(probe); got != limit || got <= defaultBodyLimit {
			t.Errorf("%s: cap %d, want its own %d", probe, got, limit)
		}
	}
	// Large enough for what the route is for: an attachment of several MiB still goes through.
	if code := post("/api/desktops/send", 5<<20); code != http.StatusOK {
		t.Errorf("an upload of 5 MiB: %d", code)
	}
}

// The resource routes refuse a JSON body that does not decode, instead of acting on what the query string said
// (RW1-E-05).
func TestResourceRoutesRefuseABodyThatDoesNotDecode(t *testing.T) {
	s := &Server{}
	big := `{"x":"` + strings.Repeat("a", maxJSONBody+1) + `"}`
	for name, h := range map[string]http.HandlerFunc{
		"increase": s.resourceIncreaseHandler,
		"dismiss":  s.resourceDismissHandler,
	} {
		for _, body := range []string{big, "{not json"} {
			req := httptest.NewRequest(http.MethodPost, "/?desktop_id=box", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h(rec, req)
			if rec.Code != http.StatusRequestEntityTooLarge && rec.Code != http.StatusBadRequest {
				t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
			}
		}
	}
}
