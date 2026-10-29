package access

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CookieName is the cookie a user's sign-in session rides in on the gated listener.
const CookieName = "decurix_user"

// Middleware identifies the person behind a request.
type Middleware struct {
	Store *Store
	// Gated reports whether a request came through the gated listener; on
	// any other request the actor is LocalOwner. Nil means every request is gated.
	Gated func(*http.Request) bool
}

// Actor returns who is acting on r: LocalOwner on the primary listener, the
// signed-in user on the gated one, and false when a gated request carries no
// valid sign-in.
func (m Middleware) Actor(r *http.Request) (User, bool) {
	if m.Gated != nil && !m.Gated(r) {
		return LocalOwner, true
	}
	if m.Store == nil {
		return User{}, false
	}
	cookie, err := r.Cookie(CookieName)
	if err != nil {
		return User{}, false
	}
	return m.Store.SessionUser(cookie.Value)
}

type actorKey struct{}

// WithActor returns ctx carrying u as the actor.
func WithActor(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, actorKey{}, u)
}

// ActorFrom returns the actor ctx carries; an automated caller carries none.
func ActorFrom(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(actorKey{}).(User)
	return u, ok
}

// Handler serves sign-in and the admin routes.
type Handler struct {
	Store      *Store
	Middleware Middleware
}

// RegisterAuth registers /api/auth/login, /api/auth/logout and /api/auth/me
// (both listeners: on the primary one they report the local owner).
func (h *Handler) RegisterAuth(mux *http.ServeMux) {
	mux.HandleFunc("/api/auth/login", h.login)
	mux.HandleFunc("/api/auth/logout", h.logout)
	mux.HandleFunc("/api/auth/me", h.me)
}

// RegisterAdmin registers the users, roles, policies and approvals-log
// routes, each wrapped by wrap (the caller's humansOnly). Primary listener only.
func (h *Handler) RegisterAdmin(mux *http.ServeMux, wrap func(http.HandlerFunc) http.HandlerFunc) {
	if wrap == nil {
		wrap = func(f http.HandlerFunc) http.HandlerFunc { return f }
	}
	routes := map[string]http.HandlerFunc{
		"/api/access/users":           h.listUsers,
		"/api/access/users/create":    h.createUser,
		"/api/access/users/update":    h.updateUser,
		"/api/access/users/delete":    h.deleteUser,
		"/api/access/policies":        h.listPolicies,
		"/api/access/policies/save":   h.savePolicy,
		"/api/access/policies/delete": h.deletePolicy,
		"/api/access/policies/check":  h.checkPolicy,
		"/api/access/workers":         h.listWorkers,
		"/api/access/workers/roles":   h.setWorkerRoles,
		"/api/access/approvals":       h.listApprovals,
	}
	for path, f := range routes {
		mux.HandleFunc(path, wrap(h.localOnly(f)))
	}
}

// localOnly refuses a request that came through the gated listener, even if a caller registered it there by mistake.
func (h *Handler) localOnly(f http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.Middleware.Gated == nil || h.Middleware.Gated(r) {
			writeError(w, http.StatusForbidden, "only from this computer")
			return
		}
		f(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, reason string) {
	writeJSON(w, status, map[string]string{"error": reason})
}

// storeError answers err with its fixed message, or a plain 500 that never echoes it.
func storeError(w http.ResponseWriter, err error) {
	if msg, ok := Message(err); ok {
		status := http.StatusBadRequest
		switch {
		case errors.Is(err, ErrNotFound):
			status = http.StatusNotFound
		case errors.Is(err, ErrNameTaken):
			status = http.StatusConflict
		case errors.Is(err, ErrSignInFailed):
			status = http.StatusUnauthorized
		case errors.Is(err, ErrLockedOut):
			status = http.StatusTooManyRequests
		}
		writeError(w, status, msg)
		return
	}
	log.Println("access:", err)
	writeError(w, http.StatusInternalServerError, "internal server error")
}

// readJSON decodes a POST's JSON body into v. JSON only: a cross-site form cannot send it without a preflight.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST required")
		return false
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "JSON required")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return false
	}
	return true
}

func requireGet(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET required")
		return false
	}
	return true
}

func (h *Handler) gated(r *http.Request) bool {
	return h.Middleware.Gated == nil || h.Middleware.Gated(r)
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if !h.gated(r) {
		writeJSON(w, http.StatusOK, map[string]any{"user": LocalOwner, "note": "no sign-in on this computer"})
		return
	}
	u, err := h.Store.Authenticate(body.Name, body.Password)
	if err != nil {
		storeError(w, err)
		return
	}
	token, expires, err := h.Store.NewSession(u.ID)
	if err != nil {
		storeError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		// Not Secure for the same reason as lanaccess's session cookie: the
		// gated listener is plain HTTP on a direct LAN address.
		SameSite: http.SameSiteStrictMode,
		Expires:  expires,
	})
	writeJSON(w, http.StatusOK, map[string]any{"user": u})
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	if cookie, err := r.Cookie(CookieName); err == nil {
		if err := h.Store.EndSession(cookie.Value); err != nil {
			log.Println("access: logout:", err)
		}
	}
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, http.StatusOK, map[string]string{"status": "signed out"})
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	if !requireGet(w, r) {
		return
	}
	u, ok := h.Middleware.Actor(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"signed_in": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"signed_in": true, "user": u})
}

func (h *Handler) listUsers(w http.ResponseWriter, r *http.Request) {
	if !requireGet(w, r) {
		return
	}
	users, err := h.Store.Users()
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

func (h *Handler) createUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     string   `json:"name"`
		Password string   `json:"password"`
		Roles    []string `json:"roles"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	u, err := h.Store.CreateUser(body.Name, body.Password, body.Roles)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": u})
}

func (h *Handler) updateUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID       string    `json:"id"`
		Roles    *[]string `json:"roles"`
		Disabled *bool     `json:"disabled"`
		Password *string   `json:"password"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if _, err := h.Store.User(body.ID); err != nil {
		storeError(w, err)
		return
	}
	if body.Roles != nil {
		if err := h.Store.SetRoles(body.ID, *body.Roles); err != nil {
			storeError(w, err)
			return
		}
	}
	if body.Disabled != nil {
		if err := h.Store.SetDisabled(body.ID, *body.Disabled); err != nil {
			storeError(w, err)
			return
		}
	}
	if body.Password != nil {
		if err := h.Store.SetPassword(body.ID, *body.Password); err != nil {
			storeError(w, err)
			return
		}
	}
	u, err := h.Store.User(body.ID)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": u})
}

func (h *Handler) deleteUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if err := h.Store.DeleteUser(body.ID); err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (h *Handler) listPolicies(w http.ResponseWriter, r *http.Request) {
	if !requireGet(w, r) {
		return
	}
	policies, err := h.Store.Policies()
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"policies": policies})
}

func (h *Handler) savePolicy(w http.ResponseWriter, r *http.Request) {
	var p Policy
	if !readJSON(w, r, &p) {
		return
	}
	saved, err := h.Store.SavePolicy(p)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"policy": saved})
}

func (h *Handler) deletePolicy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if err := h.Store.DeletePolicy(body.ID); err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// checkPolicy is a dry run: which policies would apply to an approval, without recording anything.
func (h *Handler) checkPolicy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Worker      string    `json:"worker"`
		WorkerRoles *[]string `json:"worker_roles"`
		Action      string    `json:"action"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	roles := h.Store.WorkerRoles(body.Worker)
	if body.WorkerRoles != nil {
		roles = *body.WorkerRoles
	}
	req, err := h.Store.Required(body.Worker, roles, body.Action)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"requirement": req, "approvers": describe(req)})
}

func (h *Handler) listWorkers(w http.ResponseWriter, r *http.Request) {
	if !requireGet(w, r) {
		return
	}
	all, err := h.Store.AllWorkerRoles()
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workers": all})
}

func (h *Handler) setWorkerRoles(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Worker string   `json:"worker"`
		Roles  []string `json:"roles"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if err := h.Store.SetWorkerRoles(body.Worker, body.Roles); err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"worker": body.Worker, "roles": h.Store.WorkerRoles(body.Worker)})
}

func (h *Handler) listApprovals(w http.ResponseWriter, r *http.Request) {
	if !requireGet(w, r) {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	log, err := h.Store.Approvals(strings.TrimSpace(r.URL.Query().Get("worker")), limit)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"approvals": log, "as_of": time.Now().UTC()})
}
