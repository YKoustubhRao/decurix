package httpapi

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"

	"github.com/YKoustubhRao/decurix/internal/desktop"
)

// The primary listener has no login and can create desktops, run work on them and reach the host. Three things are
// in its way since the review of 2026-11-02, which created a desktop and queued a task from a page on another site:
//
//   - A browser page from another site can send requests to 127.0.0.1:8080 (a form post or a no-cors fetch needs no
//     permission). Every request that changes something, and every WebSocket, must come from the control plane's own
//     pages: same origin, or no browser at all (a script, the Electron shell, an agent's urllib - no Origin header).
//   - A page can also rename itself onto this address (DNS rebinding) and become "same origin". The Host it sends
//     then is its own domain, so only addresses and names this listener is known by are served.
//   - A desktop reaches this listener over the Docker network to call back, and its agent takes instructions from
//     web pages and email. It may use the routes it calls back on (containerRoutes) and nothing else, and on those it
//     is who its address says it is, not who its body says (callerDesktop).

// callerKey carries the ID of the desktop a request came from (GuardPrimary sets it; nothing a caller sends can).
type callerKey struct{}

// containerRoute is a path a desktop's agent or its launchers call back on.
type containerRoute struct {
	path   string
	prefix bool
	// readOnly limits the route to GET and HEAD.
	readOnly bool
}

// containerRoutes is every control-plane route a desktop needs. Anything else from a container is refused. Adding a
// callback means adding it here, deliberately.
var containerRoutes = []containerRoute{
	// The hosted models (the key stays here) and the local one through the control plane.
	{path: "/api/llm", readOnly: true},
	{path: "/api/llm/", prefix: true},
	{path: "/api/native-ollama/", prefix: true},
	{path: "/api/grounder", readOnly: true},
	{path: "/api/grounder/locate"},
	// What the agent reports: progress, outcomes, questions for a person, memory, drafted emails.
	{path: "/api/agent/events"},
	{path: "/api/querytext"},
	{path: "/api/desktops/memory"},
	{path: "/api/desktops/learned"},
	{path: "/api/outbox"},
	// The org's rules and skills, read only.
	{path: "/api/knowledge", readOnly: true},
	{path: "/api/skillhub", readOnly: true},
	// Work handed to another worker (docs/handoff.md).
	{path: "/api/handoffs"},
	{path: "/api/handoffs/get", readOnly: true},
	{path: "/api/handoffs/cancel"},
	{path: "/api/handoffs/ask"},
	// The checks of files a task saved on this computer.
	{path: "/api/host-files/stat", readOnly: true},
	{path: "/api/host-files/read", readOnly: true},
	{path: "/api/host-files/release"},
	// Launchers for host and phone apps: each command is signed (winhost.ExecToken), each link a launcher opens in the
	// person's browser too (winhost.OpenToken), each view carries a token.
	{path: "/api/host-bridge/exec"},
	{path: "/api/host-bridge/open"},
	{path: "/api/host-gui/start"},
	{path: "/api/host-gui/stop"},
	{path: "/api/host-gui/ws"},
	{path: "/host-gui/view", readOnly: true},
	{path: "/phone-app/view", readOnly: true},
	{path: "/api/phone-app/ws"},
	{path: noVNCPath, readOnly: true},
	{path: "/favicon.svg", readOnly: true},
}

// containerMayCall reports whether a desktop may call method on path.
func containerMayCall(method, path string) bool {
	for _, route := range containerRoutes {
		if route.prefix && !strings.HasPrefix(path, route.path) || !route.prefix && path != route.path {
			continue
		}
		if route.readOnly && method != http.MethodGet && method != http.MethodHead {
			return false
		}
		return true
	}
	return false
}

// GuardPrimary wraps the primary listener: known Host names only, no cross-site writes or WebSockets, and a desktop
// only on its callback routes, as itself.
func (s *Server) GuardPrimary(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		noFraming(w)
		if !knownHost(r.Host) {
			http.Error(w, "unknown host name for this listener", http.StatusMisdirectedRequest)
			return
		}
		if changesSomething(r) && !sameOrigin(r) {
			http.Error(w, "refused a cross-site request", http.StatusForbidden)
			return
		}
		// Anything but the host is held to the desktops' callbacks, and on them must be a desktop's address: a container
		// on a network the control plane does not know is no person (review 2026-11-02, third round).
		if s.fromContainer(r) {
			if !containerMayCall(r.Method, r.URL.Path) {
				jsonStatusError(w, http.StatusForbidden, "not available from inside a desktop")
				return
			}
			id, ok := s.callerDesktop(r)
			if !ok {
				jsonStatusError(w, http.StatusForbidden, "this address is not a desktop's")
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), callerKey{}, id))
		}
		next.ServeHTTP(w, r)
	})
}

// GuardGated wraps the gated listener: no cross-site writes or WebSockets (its access-code cookie is sent along with
// a cross-site form post), and no container at all - a desktop has no business there, and must not try codes. An
// address in no known Docker network (a phone, a LAN laptop) reaches the login, which is this listener's boundary.
func (s *Server) GuardGated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		noFraming(w)
		if changesSomething(r) && !sameOrigin(r) {
			http.Error(w, "refused a cross-site request", http.StatusForbidden)
			return
		}
		if s.fromKnownContainer(r) {
			jsonStatusError(w, http.StatusForbidden, "not available from inside a desktop")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// noFraming keeps every page of the control plane out of another site's frames (clickjacking: a page that frames
// the dashboard under a decoy and gets an Approve clicked). Its own pages may still frame each other.
func noFraming(w http.ResponseWriter) {
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	w.Header().Add("Content-Security-Policy", "frame-ancestors 'self'")
}

// changesSomething reports whether r may change state: any method but GET, HEAD and OPTIONS, and any WebSocket.
func changesSomething(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
	}
	return true
}

// sameOrigin reports whether r came from one of this listener's own pages, or from no browser page at all. A
// browser sends Origin with every cross-origin request and with every POST and WebSocket; "null" is a sandboxed or
// opaque page and never ours.
func sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default:
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	// Behind a tunnel the Host may be the tunnel's local target while the page is on its public name, which the
	// tunnel passes on. A cross-site page cannot set this header (a form cannot, and a fetch that does needs a
	// preflight this listener never answers); the primary listener refuses forwarded requests outright.
	fwd := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Host"), ",")[0])
	return fwd != "" && strings.EqualFold(u.Host, fwd)
}

// knownHost reports whether host (a request's Host header) is a name this listener is known by: an IP address,
// localhost, host.docker.internal, a single-label name (a container's or the compose service's - no public DNS name
// is single-label), or one listed in DECURIX_ALLOWED_HOSTS (comma-separated). A DNS-rebinding page sends its own
// dotted domain and is refused.
func knownHost(host string) bool {
	h := host
	if hostOnly, _, err := net.SplitHostPort(host); err == nil {
		h = hostOnly
	}
	h = strings.ToLower(strings.TrimSuffix(strings.Trim(h, "[]"), "."))
	if h == "" {
		// HTTP/1.0 without Host: no browser sends that.
		return true
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return true
	}
	if h == "localhost" || strings.HasSuffix(h, ".localhost") || h == "host.docker.internal" {
		return true
	}
	if !strings.Contains(h, ".") {
		return true
	}
	for _, allowed := range strings.Split(os.Getenv("DECURIX_ALLOWED_HOSTS"), ",") {
		if allowed = strings.ToLower(strings.TrimSpace(allowed)); allowed != "" && allowed == h {
			return true
		}
	}
	return false
}

// callerDesktop is the desktop r came from, by its address on the Docker network.
func (s *Server) callerDesktop(r *http.Request) (string, bool) {
	if s.callerAt != nil {
		return s.callerAt(r)
	}
	if s.Desktops == nil {
		return "", false
	}
	addr := remoteAddr(r)
	if !addr.IsValid() {
		return "", false
	}
	return s.Desktops.DesktopAt(r.Context(), addr)
}

// requestDesktop is the desktop GuardPrimary found r came from, and false for a request from the host.
func requestDesktop(r *http.Request) (string, bool) {
	id, ok := r.Context().Value(callerKey{}).(string)
	return id, ok && id != ""
}

// actingAs is the desktop a request may act for. From a desktop it is that desktop, whatever the request claims,
// and a claim of another one is refused (false, with the answer written). From the host it is the claim.
func actingAs(w http.ResponseWriter, r *http.Request, claimed string) (string, bool) {
	caller, fromDesktop := requestDesktop(r)
	if !fromDesktop {
		return claimed, true
	}
	if c := strings.TrimSpace(claimed); c != "" && !strings.EqualFold(c, caller) && !strings.EqualFold(desktop.SanitizeID(c), caller) {
		jsonStatusError(w, http.StatusForbidden, "a desktop can only act as itself")
		return "", false
	}
	return caller, true
}
