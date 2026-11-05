package httpapi

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/YKoustubhRao/decurix/internal/desktop"
)

// Thumbnail width bounds. defaultShotWidth matches the dashboard card at a
// 2x device pixel ratio, so a retina screen gets a sharp thumbnail without
// the dashboard asking for a full-size frame.
const (
	defaultShotWidth = 480
	maxShotWidth     = 960
	minShotWidth     = 120
)

// desktopScreenshotHandler serves a JPEG of one desktop's current screen.
//
// This is what the dashboard's cards show. It exists because the cards used
// to embed a full noVNC client per desktop instead: 83 HTTP requests and a
// WebSocket each, all to the control plane's single origin, which a browser
// serves over at most 6 concurrent HTTP/1.1 connections. Two desktops were
// enough to saturate that pool, and the VNC handshake for a screen a human
// had actually opened would queue behind the thumbnails - taking ten seconds
// or failing outright. One cached JPEG per card costs one request and leaves
// the connection pool for the screens that matter.
func (s *Server) desktopScreenshotHandler(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))

	if id == "" {
		jsonStatusError(w, http.StatusBadRequest, "missing desktop id")
		return
	}

	width := defaultShotWidth

	if raw := r.URL.Query().Get("w"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			width = min(max(parsed, minShotWidth), maxShotWidth)
		}
	}

	image, err := s.Desktops.Screenshot(r.Context(), id, width)

	switch {
	case errors.Is(err, desktop.ErrNotFound):
		jsonStatusError(w, http.StatusNotFound, "desktop not found")
		return

	case errors.Is(err, desktop.ErrNotRunning),
		errors.Is(err, desktop.ErrPaused),
		errors.Is(err, desktop.ErrNoNetwork),
		errors.Is(err, desktop.ErrNoAddress):

		jsonStatusError(w, http.StatusServiceUnavailable, "desktop is not running")
		return

	case err != nil:
		// A desktop that is still booting has no Xvnc listening yet, so this
		// is a normal state rather than a fault: log it for the operator and
		// let the dashboard fall back to its placeholder.
		log.Printf("desktop screenshot for %s: %v", desktop.SanitizeID(id), err)

		jsonStatusError(w, http.StatusServiceUnavailable, "desktop screen is not available")
		return
	}

	writeScreenshot(w, r, image)
}

// writeScreenshot answers with image as a JPEG, or 304 when the caller already has it (its ETag).
func writeScreenshot(w http.ResponseWriter, r *http.Request, image []byte) {
	etag := fmt.Sprintf("\"%x\"", sha256.Sum256(image))
	w.Header().Set("ETag", etag)

	if match := r.Header.Get("If-None-Match"); match != "" {
		if strings.Contains(match, etag) || match == "*" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Length", strconv.Itoa(len(image)))
	w.Header().Set("Cache-Control", "private, no-cache")

	_, _ = w.Write(image)
}
