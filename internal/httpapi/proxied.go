package httpapi

import "net/http"

// forwardingHeaders are what a reverse proxy or tunnel (Cloudflare's edge and
// cloudflared, nginx, Caddy...) adds to a request it relays.
var forwardingHeaders = []string{"Cf-Connecting-Ip", "Cf-Ray", "X-Forwarded-For", "X-Forwarded-Host", "Forwarded", "X-Real-Ip"}

// isForwarded reports whether r arrived through a reverse proxy or tunnel.
func isForwarded(r *http.Request) bool {
	for _, h := range forwardingHeaders {
		if r.Header.Get(h) != "" {
			return true
		}
	}
	return false
}

// RefuseForwarded wraps the primary listener, which has no login and is meant
// for this machine and its own desktops only. Tunnelling it is never intended
// (the scripts tunnel the access-code-gated listener instead), so a relayed
// request is refused with a pointer to that one. Headers are a safety net for
// a stale script or a hand-run cloudflared, not the boundary: the boundary is
// that nothing is set up to publish this listener beyond loopback and the
// Docker network.
func RefuseForwarded(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isForwarded(r) {
			http.Error(w, "this listener is not available through a tunnel or proxy - use the access-code-gated address", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
