// Package lanaccess gates the second, LAN/tunnel-facing HTTP listener behind
// a rotating access code - AO (agent-orchestrator)'s "Connect Mobile" design,
// adapted to Flowera's shape: a phone reaches the control plane by opening a
// page and typing a code shown in the dashboard's Mobile menu, rather than a
// native app pairing over a QR code.
//
// This package knows nothing about the rest of Flowera on purpose: it never
// touches the primary :8080 listener, which desktop agents call back into and
// which a human on the control plane's own machine uses directly. Gating that
// one risks silently breaking every desktop's callback path; this package is
// only ever wired onto a second, separate port (see cmd/flowera/main.go),
// exactly the surface a Cloudflare tunnel or a LAN address is pointed at.
package lanaccess

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// passwordChars avoids characters easy to mistype on a phone keyboard or
	// confuse with one another: no 0/O, 1/l/I.
	passwordChars = "23456789ABCDEFGHJKMNPQRSTUVWXYZabcdefghjkmnpqrstuvwxyz"
	passwordLen   = 10

	sessionCookie = "flowera_lan_session"
	sessionTTL    = 30 * 24 * time.Hour

	// loginPath is the path the gate's own form POSTs a typed code to.
	loginPath = "/__flowera_access/login"

	// authPath grants a session from a code carried in the URL itself
	// (?code=...&next=...) instead of typed into the gate's form - what lets
	// build.sh/rebuild.sh (and their Windows twins) hand the person who just
	// ran them a link that authenticates on click, since that person already
	// has the code printed in their own terminal a moment earlier. It goes
	// through the exact same verify as a typed code, so a link is not a way
	// around the lockout.
	authPath = "/__flowera_access/authenticate"

	maxAttempts   = 5
	lockoutWindow = 5 * time.Minute
)

// Store holds the current access code and the sessions it has granted, for
// the LAN/tunnel-facing listener - the surface a phone reaches, as opposed to
// the primary listener a desktop agent's callbacks and a human at the control
// plane's own machine use (see the package doc).
type Store struct {
	mu       sync.Mutex
	password string
	sessions map[string]time.Time

	attemptsMu sync.Mutex
	attempts   map[string]*attemptState
}

// attemptState is one source's recent failed-code history, for the lockout.
type attemptState struct {
	count       int
	windowStart time.Time
	lockedUntil time.Time
}

// NewStore creates a Store with a freshly generated access code.
func NewStore() *Store {
	return &Store{
		password: generatePassword(),
		sessions: make(map[string]time.Time),
		attempts: make(map[string]*attemptState),
	}
}

// generatePassword returns a random passwordLen-character code drawn from
// passwordChars. crypto/rand failing means the OS RNG is broken; there is
// nothing safe to fall back to, so this refuses to hand out a guessable one.
func generatePassword() string {
	raw := make([]byte, passwordLen)
	if _, err := rand.Read(raw); err != nil {
		log.Fatalf("lanaccess: cannot generate an access code: %v", err)
	}

	code := make([]byte, passwordLen)
	for i, b := range raw {
		code[i] = passwordChars[int(b)%len(passwordChars)]
	}

	return string(code)
}

// Password returns the current access code, for the dashboard's Mobile menu
// to show the person sitting at the control plane. Never serve this over the
// listener the code itself gates - see httpapi's lanAccessStatusHandler,
// which is registered only on the primary mux.
func (s *Store) Password() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.password
}

// Rotate replaces the access code and drops every session it had granted -
// AO's "rotating drops the currently-connected phone", the deliberate way to
// revoke access without restarting the control plane.
func (s *Store) Rotate() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.password = generatePassword()
	s.sessions = make(map[string]time.Time)

	return s.password
}

// newSession mints and records a session token good for sessionTTL.
func (s *Store) newSession() string {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		log.Fatalf("lanaccess: cannot generate a session token: %v", err)
	}

	token := hex.EncodeToString(raw)

	s.mu.Lock()
	s.sessions[token] = time.Now().Add(sessionTTL)
	s.mu.Unlock()

	return token
}

// validSession reports whether token is a live, unexpired session, pruning
// it if it has expired.
func (s *Store) validSession(token string) bool {
	if token == "" {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	expiry, ok := s.sessions[token]
	if !ok {
		return false
	}

	if time.Now().After(expiry) {
		delete(s.sessions, token)
		return false
	}

	return true
}

// locked reports whether key is currently locked out of further attempts,
// and for how much longer.
func (s *Store) locked(key string) (bool, time.Duration) {
	s.attemptsMu.Lock()
	defer s.attemptsMu.Unlock()

	a, ok := s.attempts[key]
	if !ok || !time.Now().Before(a.lockedUntil) {
		return false, 0
	}

	return true, time.Until(a.lockedUntil)
}

// recordFailure counts a wrong code against key and locks it out once it has
// failed maxAttempts times within lockoutWindow.
//
// key is normally the request's remote address. Over a Cloudflare quick
// tunnel every request arrives from cloudflared's own local proxy connection
// - the same "127.0.0.1" key for every visitor on the internet, not one per
// attacker - so this degrades to one shared bucket for all tunnel traffic
// rather than AO's genuinely per-device lockout on a real LAN. That is a
// real weakening, not an oversight: it is why the code itself is longer than
// AO's 8 characters, to keep a brute force impractical even without a tight
// per-attacker throttle.
func (s *Store) recordFailure(key string) {
	s.attemptsMu.Lock()
	defer s.attemptsMu.Unlock()

	now := time.Now()

	a, ok := s.attempts[key]
	if !ok || now.Sub(a.windowStart) > lockoutWindow {
		a = &attemptState{windowStart: now}
		s.attempts[key] = a
	}

	a.count++

	if a.count >= maxAttempts {
		a.lockedUntil = now.Add(lockoutWindow)
		a.count = 0
		a.windowStart = now
	}
}

// clearFailures forgets key's recent failed attempts after a correct code.
func (s *Store) clearFailures(key string) {
	s.attemptsMu.Lock()
	defer s.attemptsMu.Unlock()

	delete(s.attempts, key)
}

// Middleware wraps next so every request must carry a valid session cookie,
// obtained by submitting the current access code through the page this shows
// instead. Wire it only onto the second, LAN/tunnel-facing listener - see
// the package doc.
func (s *Store) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Checked ahead of the session cookie so a stale or already-used link
		// still re-verifies its code rather than 404ing against the real mux.
		if r.Method == http.MethodGet && r.URL.Path == authPath {
			s.handleAuthenticate(w, r)
			return
		}

		if cookie, err := r.Cookie(sessionCookie); err == nil && s.validSession(cookie.Value) {
			next.ServeHTTP(w, r)
			return
		}

		if r.Method == http.MethodPost && r.URL.Path == loginPath {
			s.handleSubmit(w, r)
			return
		}

		requested := r.URL.RequestURI()
		if r.URL.Path == loginPath || requested == "" {
			requested = "/"
		}

		s.renderGate(w, http.StatusOK, requested, "")
	})
}

// verify checks code against the current password for key, applying and
// updating the same lockout bookkeeping a form submission does - shared by
// handleSubmit and handleAuthenticate so a code carried in a link is not a
// way around the brute-force protection. ok reports whether code was
// accepted; when it is not, status and errMsg are what renderGate should
// show.
func (s *Store) verify(code, key string) (ok bool, status int, errMsg string) {
	if locked, wait := s.locked(key); locked {
		return false, http.StatusTooManyRequests,
			fmt.Sprintf("Too many attempts - try again in %s.", roundDuration(wait))
	}

	if code == "" || subtle.ConstantTimeCompare([]byte(code), []byte(s.Password())) != 1 {
		s.recordFailure(key)

		// Report the lockout immediately on the failure that triggers it,
		// rather than making the person submit once more just to learn why
		// nothing happened.
		if locked, wait := s.locked(key); locked {
			return false, http.StatusTooManyRequests,
				fmt.Sprintf("Too many attempts - try again in %s.", roundDuration(wait))
		}

		return false, http.StatusOK, "That code isn't right."
	}

	s.clearFailures(key)
	return true, http.StatusOK, ""
}

// grantSession mints a session and sets it as this response's cookie - the
// last step shared by a typed code (handleSubmit) and a code carried in a
// link (handleAuthenticate).
func (s *Store) grantSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    s.newSession(),
		Path:     "/",
		HttpOnly: true,
		// Not Secure: this listener is plain HTTP by design (see the package
		// doc and cmd/flowera/main.go) so a direct LAN address works too, not
		// only the tunnel's https:// origin. Traffic through the tunnel is
		// still encrypted end-to-end by cloudflared's own protocol; only a
		// genuine direct-LAN connection is plaintext, the same deliberate
		// trade AO documents for its own LAN listener.
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(sessionTTL),
	})
}

// handleAuthenticate grants a session from ?code=&next= carried in the URL
// itself, without showing the gate's form - see authPath. A bare visit with
// no code at all (someone navigating here directly) just shows the ordinary
// gate rather than counting as a wrong attempt.
func (s *Store) handleAuthenticate(w http.ResponseWriter, r *http.Request) {
	next := sanitizeNext(r.URL.Query().Get("next"))
	code := r.URL.Query().Get("code")

	if code == "" {
		s.renderGate(w, http.StatusOK, next, "")
		return
	}

	ok, status, errMsg := s.verify(code, remoteKey(r))
	if !ok {
		s.renderGate(w, status, next, errMsg)
		return
	}

	s.grantSession(w)
	http.Redirect(w, r, next, http.StatusFound)
}

// handleSubmit validates a submitted code, applying the lockout, and either
// grants a session (redirecting on to whatever page was originally
// requested) or re-shows the gate with an error.
func (s *Store) handleSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderGate(w, http.StatusBadRequest, "/", "Something went wrong reading that - try again.")
		return
	}

	next := sanitizeNext(r.FormValue("next"))
	code := strings.TrimSpace(r.FormValue("code"))

	ok, status, errMsg := s.verify(code, remoteKey(r))
	if !ok {
		s.renderGate(w, status, next, errMsg)
		return
	}

	s.grantSession(w)
	http.Redirect(w, r, next, http.StatusFound)
}

// sanitizeNext keeps the post-login redirect on this same origin: anything
// that is not an ordinary absolute path (or that starts "//", a
// protocol-relative URL to somewhere else entirely) collapses to "/".
func sanitizeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}

	return next
}

// remoteKey extracts the request's source address, without its port, for
// the lockout - see recordFailure's caveat about what this means over a
// tunnel.
func remoteKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}

	return host
}

// roundDuration renders a wait time the way a person reads a countdown:
// whole seconds under a minute, whole minutes (rounded up) after that.
func roundDuration(d time.Duration) string {
	d = d.Round(time.Second)

	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}

	return fmt.Sprintf("%dm", int(d.Minutes())+1)
}

// gateData is what gateTmpl renders.
type gateData struct {
	Action string
	Next   string
	Error  string
}

// renderGate writes the "enter the access code" page. It carries no
// dependency on web.TemplatesFS or Flowera's shared theme on purpose - this
// package depends on nothing internal (see the package doc) - so its palette
// is a small, hand-kept copy of the dashboard's own tokens rather than a
// shared one; the two are expected to drift a little rather than pull this
// package into the module's template graph over one page.
func (s *Store) renderGate(w http.ResponseWriter, status int, next, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)

	if err := gateTmpl.Execute(w, gateData{Action: loginPath, Next: next, Error: errMsg}); err != nil {
		log.Printf("lanaccess: rendering gate page: %v", err)
	}
}

var gateTmpl = template.Must(template.New("gate").Parse(`<!DOCTYPE html>
<html lang="en"><head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0, viewport-fit=cover">
<meta name="color-scheme" content="light dark">
<meta name="robots" content="noindex">
<title>Flowera - Access code</title>
<style>
:root{--paper:#f6f5f2;--surface:#fffefc;--line:#e1dfd8;--text:#1a1917;--text-2:#5b584f;--accent:#136c4b;--accent-hover:#0f5a3e;--danger:#a03528}
@media (prefers-color-scheme:dark){:root{--paper:#100f0e;--surface:#191817;--line:#2c2a26;--text:#f2f0ea;--text-2:#a9a59a;--accent:#4fbd8b;--accent-hover:#6ad0a1;--danger:#e8836f}}
*{box-sizing:border-box}
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;background:var(--paper);color:var(--text);font-family:ui-sans-serif,system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;padding:24px}
.card{width:100%;max-width:340px;background:var(--surface);border:1px solid var(--line);border-radius:16px;padding:26px}
h1{font-size:17px;margin:0 0 6px}
p{font-size:13px;color:var(--text-2);margin:0 0 18px;line-height:1.5}
input{width:100%;padding:11px 12px;font-size:16px;border:1px solid var(--line);border-radius:9px;background:var(--paper);color:var(--text);letter-spacing:.03em}
input:focus{outline:2px solid var(--accent);outline-offset:1px}
button{width:100%;margin-top:12px;padding:11px;font-size:14px;font-weight:600;border:0;border-radius:9px;background:var(--accent);color:#fff;cursor:pointer}
button:hover{background:var(--accent-hover)}
.err{margin:0 0 14px;font-size:12.5px;color:var(--danger)}
</style>
</head><body>
<form class="card" method="POST" action="{{.Action}}" autocomplete="off">
  <h1>Enter the access code</h1>
  <p>Read it off the Flowera dashboard's Mobile menu, on the machine running your control plane.</p>
  {{if .Error}}<p class="err">{{.Error}}</p>{{end}}
  <input type="text" name="code" placeholder="Access code" autocapitalize="off" autocomplete="off" spellcheck="false" autofocus required>
  <input type="hidden" name="next" value="{{.Next}}">
  <button type="submit">Continue</button>
</form>
</body></html>`))
