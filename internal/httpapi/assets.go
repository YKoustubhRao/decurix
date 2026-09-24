package httpapi

import (
	"bytes"
	"compress/gzip"
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

// noVNCPath is where the screen viewer's RFB client is served. The version is
// in the path, so the file can be cached for good: a new noVNC is a new path.
const noVNCPath = "/static/novnc/rfb-1.6.0.js"

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
