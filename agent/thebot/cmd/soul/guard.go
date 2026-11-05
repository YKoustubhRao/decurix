package main

import (
	"crypto/subtle"
	"mime"
	"net/http"
	"strings"
)

// healthPath is the one API route answered without the agent token: it says
// only that soul is up.
const healthPath = "/api/health"

// guard is who may call soul, decided before any route runs. Soul runs
// commands and resumes paused approvals, so a caller that is not the control
// plane (through its /agent/<id>/ proxy, which adds the desktop's token) or
// this desktop's own agent must not get through:
//   - a web page: a page open in the worker's Chrome can POST to soul as
//     text/plain with no CORS preflight. Every write must be
//     application/json (which a page can only send cross-origin after a
//     preflight soul never answers), and a request carrying an Origin that
//     is not soul's own (the Host it was sent to), or a Sec-Fetch-Site that
//     says another site sent it, is refused.
//   - another desktop on the shared Docker network: every API route but the
//     health check needs "Authorization: Bearer <DECURIX_AGENT_TOKEN>",
//     compared in constant time. Without a token soul fails closed: those
//     routes are refused until the desktop is recreated with one.
//
// The honest limit: the worker's own shell runs as the same user and can read
// the token from its environment. That is what the approval gate is for: no
// request can approve itself (handleChat takes no "approved" field), and a
// yes reaches a job only as the reply to its own paused approval.
func (s *server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status, why := s.refusal(r); status != 0 {
			writeJSON(w, status, map[string]any{"error": why})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// refusal is the status and reason a request is refused with, or 0.
func (s *server) refusal(r *http.Request) (int, string) {
	if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r.Host) {
		return http.StatusForbidden, "requests from another site are refused"
	}
	switch strings.ToLower(r.Header.Get("Sec-Fetch-Site")) {
	case "", "same-origin", "none":
	default:
		return http.StatusForbidden, "requests from another site are refused"
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/json" {
			return http.StatusUnsupportedMediaType, "a write must be sent as application/json"
		}
	}
	if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != healthPath {
		token := s.cfg.AgentToken
		if token == "" {
			// Fail closed: a desktop created before agent tokens existed (or by a
			// control plane whose secret did not load) has no token, and without
			// one anyone on the Docker network could run commands here.
			return http.StatusUnauthorized, noTokenRefusal
		}
		if !bearerMatches(r.Header.Get("Authorization"), token) {
			return http.StatusUnauthorized, "this desktop's agent token is required"
		}
	}
	return 0, ""
}

// noTokenRefusal is what every API route but the health check answers on a
// desktop that has no agent token.
const noTokenRefusal = "this desktop was created without an agent token: recreate it"

// sameOrigin reports whether origin is the address the request was sent to
// (host, the request's Host): a page served from soul itself, or from the
// control plane that proxies it.
func sameOrigin(origin, host string) bool {
	if host == "" {
		return false
	}
	o := strings.ToLower(strings.TrimSuffix(origin, "/"))
	h := strings.ToLower(host)
	return o == "http://"+h || o == "https://"+h
}

// bearerMatches reports whether an Authorization header carries token, in
// constant time.
func bearerMatches(header, token string) bool {
	const prefix = "Bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	got := strings.TrimSpace(header[len(prefix):])
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}
