package httpapi

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"

	"github.com/YKoustubhRao/decurix/internal/webui"
)

// faviconHandler serves the embedded Decurix mark as the site favicon, which
// every page points at with <link rel="icon"> in the shared "theme" template.
func (s *Server) faviconHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(webui.FaviconSVG)
}

// noVNCPath is where the screen viewer's RFB client is served. The version and
// a hash of the file's own bytes are in the path, so the file can be cached for
// good: a changed file is a new path even if the version was not bumped
// (follow-up W-20: a version-only path kept a stale copy in every browser).
var noVNCPath = "/static/novnc/rfb-1.6.0-" + contentTag(webui.NoVNC) + ".js"

// contentTag is the first 12 hex digits of data's SHA-256.
func contentTag(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:6])
}

var (
	noVNCGzipOnce sync.Once
	noVNCGzip     []byte
)

// noVNCHandler serves the bundled noVNC client (web/novnc) that every
// desktop's screen viewer loads: once per browser, gzipped where the browser
// takes it (185 KB -> 56 KB), cached for a year.
func (s *Server) noVNCHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Vary", "Accept-Encoding")

	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		noVNCGzipOnce.Do(func() {
			var buf bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
			zw.Write(webui.NoVNC)
			zw.Close()
			noVNCGzip = buf.Bytes()
		})
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(noVNCGzip)
		return
	}

	w.Write(webui.NoVNC)
}
