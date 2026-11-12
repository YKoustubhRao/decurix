// hostguard.go keeps a Brainiac without an API key to this machine's own
// programs. With no key, loopback is the only guard, and a web page in the
// person's browser can still reach loopback: through DNS rebinding (a name
// the attacker controls that resolves to 127.0.0.1) or, on Windows, through
// WSL's forwarded localhost. Such a request carries the attacker's name in
// its Host header, or the attacker's page in its Origin header, so both
// must name this machine (or a name the operator allowed).
package api

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// Fixed refusals: they never repeat the Host or Origin that was sent.
const (
	msgBadHost   = "Brainiac without an API key answers only on 127.0.0.1, ::1 or localhost (or a name in allowed_hosts)"
	msgBadOrigin = "Brainiac without an API key does not answer requests from web pages on other sites"
)

// guardLocal refuses, with 403, a request that did not come from a program on
// this machine; true means it may go on. It runs only when no API key is set.
//
// /healthz is checked too: open to every Host, it would let a page behind a
// rebinding name learn that Brainiac runs here, and every caller of it
// (`brainiac update`, scripts) uses the listen address, which passes.
func (s *Server) guardLocal(w http.ResponseWriter, r *http.Request) bool {
	if !s.localName(hostname(r.Host)) {
		writeError(w, http.StatusForbidden, "forbidden_host", msgBadHost)
		return false
	}
	// A request without Origin is a program (curl, an SDK, Decurix's
	// clients); a browser sends Origin on every cross-site POST, so a page
	// cannot make this machine compute even an answer it cannot read.
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil || u.Host == "" || !s.localName(hostname(u.Host)) {
			writeError(w, http.StatusForbidden, "forbidden_origin", msgBadOrigin)
			return false
		}
	}
	return true
}

// localName reports whether a hostname is this machine (localhost, 127.0.0.0/8,
// ::1) or one the operator allowed. "*.localhost" is not taken: not every
// resolver keeps it on loopback.
func (s *Server) localName(h string) bool {
	if h == "" {
		return false
	}
	if h == "localhost" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
		return true
	}
	for _, a := range s.AllowedHosts {
		if a = hostname(strings.TrimSpace(a)); a != "" && a == h {
			return true
		}
	}
	return false
}

// hostname is a Host value's name, lower case, without port or brackets.
func hostname(hostport string) string {
	h := hostport
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		h = host
	}
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	return strings.ToLower(h)
}
