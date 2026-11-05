package handoff

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxBodyBytes bounds a handoff request's JSON body.
const maxBodyBytes = 96 << 10

// Handler serves the handoff routes. A worker names itself the way every
// agent callback to the control plane does: desktop_id (or from_worker) in
// the body. Register it on the primary listener only.
type Handler struct {
	Service *Service
	// AskWaitCeiling caps how long POST /api/handoffs/ask holds the request
	// open waiting for the answer (default 60 s); the caller polls after that.
	AskWaitCeiling time.Duration
	// Caller says which worker sent r, from something the caller cannot choose (its address), and false for a
	// request that is not a worker's (a person's, on this machine). A worker then acts only as itself: a name in the
	// body is not proof (review 2026-11-02). Nil trusts the body, as tests and a single-desktop setup may.
	Caller func(r *http.Request) (string, bool)
}

// worker is who r acts for: the caller when it is a worker (refusing a claim of another, ok=false with the answer
// written), else the claim.
func (h *Handler) worker(w http.ResponseWriter, r *http.Request, claimed string) (string, bool) {
	if h.Caller == nil {
		return claimed, true
	}
	caller, isWorker := h.Caller(r)
	if !isWorker {
		return claimed, true
	}
	if c := strings.TrimSpace(claimed); c != "" && !strings.EqualFold(c, caller) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "a worker can only act as itself"})
		return "", false
	}
	return caller, true
}

// Register adds the handoff routes to mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/handoffs", h.handoffs)
	mux.HandleFunc("/api/handoffs/get", h.get)
	mux.HandleFunc("/api/handoffs/cancel", h.cancel)
	mux.HandleFunc("/api/handoffs/ask", h.ask)
}

// writeJSON writes v as JSON with status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError answers with a curated reason: an InputError's own message, a
// 404 for a missing handoff, else a generic 500 (never err.Error()).
func writeError(w http.ResponseWriter, err error) {
	var in *InputError
	switch {
	case errors.As(err, &in):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": in.Msg})
	case errors.Is(err, ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "handoff not found"})
	default:
		log.Printf("handoff: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal server error"})
	}
}

// decode reads a JSON body into v, answering 400 itself when it cannot.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the body is not a valid handoff request"})
		return false
	}
	return true
}

func firstOf(a ...string) string {
	for _, s := range a {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return ""
}

// handoffs is POST /api/handoffs (create) and GET /api/handoffs?worker= (list).
func (h *Handler) handoffs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		who, ok := h.worker(w, r, strings.TrimSpace(r.URL.Query().Get("worker")))
		if !ok {
			return
		}
		list, err := h.Service.Store.List(who, atoi(r.URL.Query().Get("limit")))
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"handoffs": list})
	case http.MethodPost:
		var req struct {
			DesktopID  string            `json:"desktop_id"`
			FromWorker string            `json:"from_worker"`
			FromTask   string            `json:"from_task"`
			ToWorker   string            `json:"to_worker"`
			ToRole     string            `json:"to_role"`
			ToSkill    string            `json:"to_skill"`
			Request    string            `json:"request"`
			Inputs     map[string]string `json:"inputs"`
			Files      []string          `json:"files"`
			Expected   string            `json:"expected"`
		}
		if !decode(w, r, &req) {
			return
		}
		from, ok := h.worker(w, r, firstOf(req.DesktopID, req.FromWorker))
		if !ok {
			return
		}
		out, err := h.Service.Delegate(r.Context(), DelegateRequest{
			FromWorker: from, FromTask: req.FromTask,
			ToWorker: req.ToWorker, ToRole: req.ToRole, ToSkill: req.ToSkill,
			Request: req.Request, Inputs: req.Inputs, Files: req.Files, Expected: req.Expected,
		})
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, out)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// get is GET /api/handoffs/get?id=: the handoff, brought up to date.
func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id is required"})
		return
	}
	if h.Caller != nil {
		if caller, isWorker := h.Caller(r); isWorker {
			// A worker sees the handoffs it sent or was sent, no one else's.
			found, err := h.Service.Store.Get(id)
			if err != nil {
				writeError(w, err)
				return
			}
			if !strings.EqualFold(found.FromWorker, caller) && !strings.EqualFold(found.ToWorker, caller) {
				writeError(w, ErrNotFound)
				return
			}
		}
	}
	out, err := h.Service.Get(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// cancel is POST /api/handoffs/cancel {id, desktop_id?}.
func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req struct {
		ID         string `json:"id"`
		DesktopID  string `json:"desktop_id"`
		FromWorker string `json:"from_worker"`
	}
	if !decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.ID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id is required"})
		return
	}
	who, ok := h.worker(w, r, firstOf(req.DesktopID, req.FromWorker))
	if !ok {
		return
	}
	out, err := h.Service.Cancel(r.Context(), strings.TrimSpace(req.ID), who)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ask is POST /api/handoffs/ask {from_worker|desktop_id, from_task, skill,
// question, wait}: the question goes to a free worker holding the skill, and
// the answer comes back within wait seconds (capped) when it can; otherwise
// the open handoff is returned for the caller to poll.
func (h *Handler) ask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req struct {
		DesktopID  string  `json:"desktop_id"`
		FromWorker string  `json:"from_worker"`
		FromTask   string  `json:"from_task"`
		Skill      string  `json:"skill"`
		Question   string  `json:"question"`
		Wait       float64 `json:"wait"`
	}
	if !decode(w, r, &req) {
		return
	}
	asker, ok := h.worker(w, r, firstOf(req.DesktopID, req.FromWorker))
	if !ok {
		return
	}
	out, err := h.Service.Ask(r.Context(), AskRequest{
		FromWorker: asker, FromTask: req.FromTask,
		Skill: req.Skill, Question: req.Question,
	})
	if err != nil {
		writeError(w, err)
		return
	}

	ceiling := orDefault(h.AskWaitCeiling, defaultAskWaitCeiling)
	wait := time.Duration(req.Wait * float64(time.Second))
	if wait > ceiling {
		wait = ceiling
	}
	out = h.waitFor(r.Context(), out, wait)

	writeJSON(w, http.StatusOK, map[string]any{
		"handoff":     out,
		"answered_by": answeredBy(out),
		"answer":      out.Result.Report,
		"verified":    out.Result.Verified,
	})
}

// waitFor polls an open handoff until it closes or wait passes.
func (h *Handler) waitFor(ctx context.Context, out Handoff, wait time.Duration) Handoff {
	deadline := time.Now().Add(wait)
	for out.Open() && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return out
		case <-time.After(time.Second):
		}
		next, err := h.Service.Get(ctx, out.ID)
		if err != nil {
			return out
		}
		out = next
	}
	return out
}

// answeredBy is the worker whose answer came back, or "".
func answeredBy(h Handoff) string {
	if h.Open() || h.Result.Report == "" {
		return ""
	}
	return h.ToWorker
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}
