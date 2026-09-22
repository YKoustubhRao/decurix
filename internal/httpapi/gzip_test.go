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

func TestGzipNegotiation(t *testing.T) {
	for _, tt := range []struct {
		value string
		want  bool
	}{
		{"gzip", true}, {"gzip;q=0", false}, {"br, gzip;q=0.5", true},
		{"notgzip", false}, {"*;q=1, gzip;q=0", false}, {"*", true},
		{"gzip;q=NaN", false}, {"gzip;q=2", false}, {"gzip;q=bad", false},
	} {
		t.Run(tt.value, func(t *testing.T) {
			if got := acceptsGzip(tt.value); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGzipPreservesUncompressedResponses(t *testing.T) {
	for _, tt := range []struct {
		name, method, contentType, encoding string
		status                              int
	}{
		{"encoded", "GET", "text/plain", "br", 200},
		{"events", "GET", "text/event-stream", "", 200},
		{"partial", "GET", "text/plain", "", 206},
		{"head", "HEAD", "text/plain", "", 200},
		{"empty", "GET", "text/plain", "", 204},
		{"not modified", "GET", "text/plain", "", 304},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/", nil)
			req.Header.Set("Accept-Encoding", "gzip")
			rec := httptest.NewRecorder()
			GzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tt.contentType)
				if tt.encoding != "" {
					w.Header().Set("Content-Encoding", tt.encoding)
				}
				w.WriteHeader(tt.status)
				if tt.method != "HEAD" && tt.status != 204 && tt.status != 304 {
					_, _ = w.Write([]byte("payload"))
				}
			})).ServeHTTP(rec, req)
			if got := rec.Header().Get("Content-Encoding"); got != tt.encoding {
				t.Fatalf("encoding %q, want %q", got, tt.encoding)
			}
			if !strings.Contains(rec.Header().Get("Vary"), "Accept-Encoding") {
				t.Fatal("missing Vary")
			}
		})
	}
}

func TestGzipFlushBeforeWrite(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	GzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte("streamed"))
	})).ServeHTTP(rec, req)
	if rec.Result().Header.Get("Content-Encoding") != "gzip" {
		t.Fatal("flush committed headers without encoding")
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	data, err := io.ReadAll(zr)
	if err != nil || string(data) != "streamed" {
		t.Fatalf("body %q, err %v", data, err)
	}
}
