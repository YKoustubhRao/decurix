package httpapi

import (
	"bufio"
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

var gzipWriterPool = sync.Pool{
	New: func() any {
		w, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
		return w
	},
}

// GzipMiddleware transparently compresses HTTP responses using gzip when
// accepted by the client, significantly reducing transfer size for HTML, JSON,
// JS, CSS, and SVG assets.
func GzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Do not compress WebSockets, SSE or requests where client doesn't accept gzip.
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") ||
			strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
			strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			next.ServeHTTP(w, r)
			return
		}

		gzWriter := gzipWriterPool.Get().(*gzip.Writer)
		defer gzipWriterPool.Put(gzWriter)

		gzw := &gzipResponseWriter{
			ResponseWriter: w,
			gz:             gzWriter,
		}
		defer gzw.close()

		next.ServeHTTP(gzw, r)
	})
}

type gzipResponseWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	wroteHeader bool
	compress    bool
}

func (g *gzipResponseWriter) WriteHeader(status int) {
	if g.wroteHeader {
		return
	}
	g.wroteHeader = true

	// Check if this response should be compressed.
	ct := g.Header().Get("Content-Type")
	if shouldCompressContentType(ct) && status != http.StatusNoContent && status != http.StatusNotModified {
		g.compress = true
		g.Header().Set("Content-Encoding", "gzip")
		g.Header().Del("Content-Length")
		g.Header().Add("Vary", "Accept-Encoding")
		g.gz.Reset(g.ResponseWriter)
	}

	g.ResponseWriter.WriteHeader(status)
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	if !g.wroteHeader {
		if g.Header().Get("Content-Type") == "" {
			g.Header().Set("Content-Type", http.DetectContentType(b))
		}
		g.WriteHeader(http.StatusOK)
	}

	if g.compress {
		return g.gz.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

func (g *gzipResponseWriter) close() {
	if g.compress {
		_ = g.gz.Close()
	}
}

func (g *gzipResponseWriter) Flush() {
	if g.compress {
		_ = g.gz.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (g *gzipResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := g.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

func shouldCompressContentType(ct string) bool {
	if ct == "" {
		return true // Default to compress text/data payloads
	}
	lower := strings.ToLower(ct)
	return strings.HasPrefix(lower, "text/") ||
		strings.Contains(lower, "application/json") ||
		strings.Contains(lower, "application/javascript") ||
		strings.Contains(lower, "application/xml") ||
		strings.Contains(lower, "image/svg+xml")
}
