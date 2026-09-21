package httpapi

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGzipMiddlewareCompressesText(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(strings.Repeat("Hello Decurix Desktop World! ", 100)))
	})

	wrapped := GzipMiddleware(handler)

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	rec := httptest.NewRecorder()

	wrapped.ServeHTTP(rec, req)

	res := rec.Result()
	if res.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("expected Content-Encoding gzip, got %s", res.Header.Get("Content-Encoding"))
	}

	gzReader, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("failed to decode gzip body: %v", err)
	}
	defer gzReader.Close()

	decompressed, err := io.ReadAll(gzReader)
	if err != nil {
		t.Fatalf("failed to read decompressed body: %v", err)
	}

	if !bytes.Contains(decompressed, []byte("Hello Decurix Desktop World!")) {
		t.Errorf("decompressed content mismatch")
	}
}

func TestGzipMiddlewarePassesThroughWithoutAcceptEncoding(t *testing.T) {
	raw := []byte("plain text content")
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(raw)
	})

	wrapped := GzipMiddleware(handler)

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()

	wrapped.ServeHTTP(rec, req)

	res := rec.Result()
	if res.Header.Get("Content-Encoding") != "" {
		t.Fatalf("expected no Content-Encoding, got %s", res.Header.Get("Content-Encoding"))
	}
	if !bytes.Equal(rec.Body.Bytes(), raw) {
		t.Errorf("body content mismatch")
	}
}
