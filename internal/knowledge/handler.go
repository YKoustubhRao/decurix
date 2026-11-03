package knowledge

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
)

// maxRequestBytes bounds a request body to these routes.
const maxRequestBytes = 64 << 10

// RoleSource tells which roles a worker carries. *skillhub.Store satisfies it.
type RoleSource interface {
	WorkerRoles(worker string) ([]string, error)
}

// Handler serves the knowledge routes.
type Handler struct {
	Store *Store
	// Roles, when set, supplies a worker's roles for GET /api/knowledge.
	Roles RoleSource
	// Caller names the desktop a request came from (ok false for a person). A desktop reads only its own scope: the
	// worker it asks for is itself, whatever it named (review 2026-11-03, C-09).
	Caller func(*http.Request) (string, bool)
	// TaskOf reports whether task is one of worker's tasks. A desktop's request for a task's knowledge is answered only
	// when it is; without TaskOf, a desktop gets no task's knowledge.
	TaskOf func(worker, task string) bool
}

// Register adds the knowledge routes to mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/knowledge", h.root)
	mux.HandleFunc("/api/knowledge/supersede", h.supersede)
	mux.HandleFunc("/api/knowledge/why", h.why)
}

// root routes GET (what a worker is given) and POST (add an item).
func (h *Handler) root(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.forWorker(w, r)
	case http.MethodPost:
		h.add(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// forWorker answers GET /api/knowledge?worker=&task=: org, then the worker's
// roles, then the worker's own items, then the task's when task is given -
// both as items and as the lines a worker is told.
func (h *Handler) forWorker(w http.ResponseWriter, r *http.Request) {
	worker := strings.TrimSpace(r.URL.Query().Get("worker"))
	task := strings.TrimSpace(r.URL.Query().Get("task"))
	if (worker != "" && !keyPattern.MatchString(worker)) || (task != "" && !keyPattern.MatchString(task)) {
		writeError(w, http.StatusBadRequest, "bad worker or task id")
		return
	}
	if h.Caller != nil {
		if caller, fromDesktop := h.Caller(r); fromDesktop {
			worker = caller
			if task != "" && (h.TaskOf == nil || !h.TaskOf(caller, task)) {
				writeError(w, http.StatusForbidden, "a desktop reads the knowledge of its own tasks only")
				return
			}
		}
	}

	roles := []string{}
	if worker != "" && h.Roles != nil {
		got, err := h.Roles.WorkerRoles(worker)
		if err != nil {
			// Roles are an addition: without them the worker still gets org and its own items.
			log.Printf("knowledge: roles of %s: %v", worker, err)
		} else {
			roles = got
		}
	}

	items, err := h.Store.ForWorker(worker, roles)
	if err != nil {
		storeError(w, err)
		return
	}
	if task != "" {
		own, err := h.Store.ForTask(task)
		if err != nil {
			storeError(w, err)
			return
		}
		items = append(items, own...)
	}

	lines := make([]string, 0, len(items))
	for _, it := range items {
		lines = append(lines, it.Line())
	}

	writeJSON(w, http.StatusOK, map[string]any{"worker": worker, "roles": roles, "items": items, "lines": lines})
}

// addRequest is the body of POST /api/knowledge and /api/knowledge/supersede.
type addRequest struct {
	ID       string `json:"id"`
	Scope    string `json:"scope"`
	ScopeKey string `json:"scope_key"`
	Kind     string `json:"kind"`
	Text     string `json:"text"`
	Source   Source `json:"source"`
	Retire   bool   `json:"retire"`
}

// add answers POST /api/knowledge {scope, scope_key, kind, text, source}.
func (h *Handler) add(w http.ResponseWriter, r *http.Request) {
	req, ok := decode(w, r)
	if !ok {
		return
	}

	it, err := h.Store.Add(Item{Scope: req.Scope, ScopeKey: req.ScopeKey, Kind: req.Kind, Text: req.Text, Source: req.Source})
	if err != nil {
		storeError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, it)
}

// supersede answers POST /api/knowledge/supersede {id, text, kind?, source},
// or {id, retire: true} to stop giving an item out with no replacement.
func (h *Handler) supersede(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	req, ok := decode(w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(req.ID) == "" {
		writeError(w, http.StatusBadRequest, "missing id")
		return
	}

	if req.Retire {
		if err := h.Store.Retire(req.ID); err != nil {
			storeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": req.ID, "retired": true})
		return
	}

	it, err := h.Store.Supersede(req.ID, Item{Kind: req.Kind, Text: req.Text, Source: req.Source})
	if err != nil {
		storeError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"item": it, "replaced": req.ID})
}

// why answers GET /api/knowledge/why?id=: the item and the items it replaced,
// newest first, each with its source.
func (h *Handler) why(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	it, chain, err := h.Store.Why(strings.TrimSpace(r.URL.Query().Get("id")))
	if err != nil {
		storeError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"item": it, "replaced": chain})
}

// decode reads a JSON body of at most maxRequestBytes.
func decode(w http.ResponseWriter, r *http.Request) (addRequest, bool) {
	var req addRequest

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "the request body is not the JSON this route takes")
		return req, false
	}

	return req, true
}

// storeError answers a store error: refused input and unknown items get the
// store's own words, anything else a generic line and a log entry.
func storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalid):
		writeError(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": "))
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "no such knowledge item")
	default:
		log.Printf("knowledge: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// writeJSON answers status with v as JSON.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError answers status with {"error": reason}.
func writeError(w http.ResponseWriter, status int, reason string) {
	writeJSON(w, status, map[string]string{"error": reason})
}
