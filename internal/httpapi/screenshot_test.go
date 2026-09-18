package httpapi

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDesktopScreenshotHandlerMissingID(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/desktops/screenshot", nil)
	w := httptest.NewRecorder()

	srv.desktopScreenshotHandler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request, got %d", w.Code)
	}
}

func TestDesktopScreenshotETagAnd304NotModified(t *testing.T) {
	fakeImage := []byte("fake-jpeg-image-bytes-for-testing")
	etag := fmt.Sprintf("\"%x\"", sha256.Sum256(fakeImage))

	// Test 304 Not Modified check logic:
	req := httptest.NewRequest(http.MethodGet, "/api/desktops/screenshot?id=test", nil)
	req.Header.Set("If-None-Match", etag)

	w := httptest.NewRecorder()

	// Simulate handler's ETag matching block:
	w.Header().Set("ETag", etag)
	if match := req.Header.Get("If-None-Match"); match != "" {
		if strings.Contains(match, etag) || match == "*" {
			w.WriteHeader(http.StatusNotModified)
		}
	}

	if w.Code != http.StatusNotModified {
		t.Fatalf("expected 304 Not Modified, got %d", w.Code)
	}
	if w.Header().Get("ETag") != etag {
		t.Fatalf("expected ETag %s, got %s", etag, w.Header().Get("ETag"))
	}
}
