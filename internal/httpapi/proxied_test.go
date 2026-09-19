package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRefuseForwarded(t *testing.T) {
	h := RefuseForwarded(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))

	direct := httptest.NewRecorder()
	h.ServeHTTP(direct, httptest.NewRequest(http.MethodGet, "/", nil))
	if direct.Code != http.StatusOK {
		t.Fatalf("direct request = %d, want 200", direct.Code)
	}

	for _, header := range forwardingHeaders {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set(header, "203.0.113.9")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: got %d, want 403", header, rec.Code)
		}
	}
}
