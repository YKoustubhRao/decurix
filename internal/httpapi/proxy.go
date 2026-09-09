package httpapi

import (
	"errors"
	"log"
	"net/http"
	"net/http/httputil"
	"strings"

	"github.com/YKoustubhRao/flowera/internal/desktop"
)

// desktopProxyHandler reverse-proxies HTTP requests to the desktop container's internal address.
func (s *Server) desktopProxyHandler(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(
		strings.TrimPrefix(r.URL.Path, "/desktop/"),
		"/",
		2,
	)

	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}

	proxyURL, desktopID, err := s.Desktops.ProxyTarget(r.Context(), parts[0])

	switch {
	case errors.Is(err, desktop.ErrNotFound):
		http.NotFound(w, r)
		return

	case errors.Is(err, desktop.ErrNotRunning):
		http.Error(
			w,
			"desktop is not running",
			http.StatusServiceUnavailable,
		)
		return

	case errors.Is(err, desktop.ErrNoNetwork):
		http.Error(
			w,
			"desktop is not connected to Flowera network",
			http.StatusServiceUnavailable,
		)
		return

	case errors.Is(err, desktop.ErrNoAddress):
		http.Error(
			w,
			"desktop has no network IP",
			http.StatusServiceUnavailable,
		)
		return

	case err != nil:
		http.Error(
			w,
			err.Error(),
			http.StatusInternalServerError,
		)
		return
	}

	reverseProxy := httputil.NewSingleHostReverseProxy(proxyURL)

	// Remove /desktop/<id> from the URL.
	prefix := "/desktop/" + desktopID

	r.URL.Path = strings.TrimPrefix(
		r.URL.Path,
		prefix,
	)

	if r.URL.Path == "" || r.URL.Path == "/" {
		r.URL.Path = "/vnc.html"
	}

	// Preserve query parameters.
	reverseProxy.ErrorHandler = func(
		rw http.ResponseWriter,
		req *http.Request,
		err error,
	) {
		log.Printf(
			"desktop proxy error for %s: %v",
			desktopID,
			err,
		)

		http.Error(
			rw,
			"desktop unavailable",
			http.StatusBadGateway,
		)
	}

	reverseProxy.ServeHTTP(w, r)
}
