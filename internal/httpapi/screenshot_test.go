package httpapi

import (
	"net/http"
	"net/http/httptest"
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

// The handler's own answer (writeScreenshot), not a copy of it: the same picture again is a 304, a new one is sent.
func TestDesktopScreenshotETagAnd304NotModified(t *testing.T) {
	image := []byte("fake-jpeg-image-bytes-for-testing")

	first := httptest.NewRecorder()
	writeScreenshot(first, httptest.NewRequest(http.MethodGet, "/api/desktops/screenshot?id=test", nil), image)
	etag := first.Header().Get("ETag")
	if first.Code != http.StatusOK || etag == "" || first.Body.String() != string(image) {
		t.Fatalf("first answer: %d etag=%q body=%q", first.Code, etag, first.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/api/desktops/screenshot?id=test", nil)
	req.Header.Set("If-None-Match", etag)
	same := httptest.NewRecorder()
	writeScreenshot(same, req, image)
	if same.Code != http.StatusNotModified || same.Body.Len() != 0 {
		t.Fatalf("same picture: %d with %d bytes, want 304 and none", same.Code, same.Body.Len())
	}

	changed := httptest.NewRecorder()
	writeScreenshot(changed, req, []byte("another picture"))
	if changed.Code != http.StatusOK || changed.Header().Get("ETag") == etag {
		t.Fatalf("changed picture: %d etag=%q", changed.Code, changed.Header().Get("ETag"))
	}
}
