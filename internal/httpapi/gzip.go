package httpapi

import (
	"bufio"
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"strconv"
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
		w.Header().Add("Vary", "Accept-Encoding")
		// Do not compress WebSockets, SSE or requests where client doesn't accept gzip.
		if r.Method == http.MethodHead || !acceptsGzip(strings.Join(r.Header.Values("Accept-Encoding"), ",")) ||
			strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
			strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			next.ServeHTTP(w, r)
			return
		}

		gzw := &gzipResponseWriter{
			ResponseWriter: w,
		}
		defer gzw.close()

		next.ServeHTTP(gzw, r)
	})
}

func acceptsGzip(value string) bool {
	wildcard := false
	for _, item := range strings.Split(value, ",") {
		parts := strings.Split(item, ";")
		coding := strings.TrimSpace(parts[0])
		q := 1.0
		for _, param := range parts[1:] {
			key, value, ok := strings.Cut(strings.TrimSpace(param), "=")
			if ok && strings.EqualFold(strings.TrimSpace(key), "q") {
				var err error
				q, err = strconv.ParseFloat(strings.TrimSpace(value), 64)
				if err != nil || !(q >= 0 && q <= 1) {
					q = 0
				}
			}
		}
		if strings.EqualFold(coding, "gzip") {
			return q > 0
		}
		if coding == "*" {
			wildcard = q > 0
		}
	}
	return wildcard
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
	if status >= 100 && status < 200 {
		g.ResponseWriter.WriteHeader(status)
		return
	}
	g.wroteHeader = true

	// Check if this response should be compressed.
	ct := g.Header().Get("Content-Type")
	if shouldCompressContentType(ct) && g.Header().Get("Content-Encoding") == "" &&
		g.Header().Get("Content-Range") == "" && status != http.StatusPartialContent &&
		status != http.StatusNoContent && status != http.StatusNotModified {
		g.compress = true
		g.Header().Set("Content-Encoding", "gzip")
		g.Header().Del("Content-Length")
		g.gz = gzipWriterPool.Get().(*gzip.Writer)
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
		g.gz.Reset(io.Discard)
		gzipWriterPool.Put(g.gz)
	}
}

func (g *gzipResponseWriter) Flush() {
	if !g.wroteHeader {
		g.WriteHeader(http.StatusOK)
	}
	if g.compress {
		_ = g.gz.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (g *gzipResponseWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }

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
	if strings.HasPrefix(lower, "text/event-stream") {
		return false
	}
	return strings.HasPrefix(lower, "text/") ||
		strings.Contains(lower, "application/json") ||
		strings.Contains(lower, "application/javascript") ||
		strings.Contains(lower, "application/xml") ||
		strings.Contains(lower, "image/svg+xml")
}
