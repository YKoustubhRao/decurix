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
		list, err := h.Service.Store.List(strings.TrimSpace(r.URL.Query().Get("worker")), atoi(r.URL.Query().Get("limit")))
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
		out, err := h.Service.Delegate(r.Context(), DelegateRequest{
			FromWorker: firstOf(req.DesktopID, req.FromWorker), FromTask: req.FromTask,
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
	out, err := h.Service.Cancel(r.Context(), strings.TrimSpace(req.ID), firstOf(req.DesktopID, req.FromWorker))
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
	out, err := h.Service.Ask(r.Context(), AskRequest{
		FromWorker: firstOf(req.DesktopID, req.FromWorker), FromTask: req.FromTask,
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
