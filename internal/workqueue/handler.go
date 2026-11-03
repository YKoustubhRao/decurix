package workqueue

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxBody bounds a request body: an item's files arrive base64-encoded.
const maxBody = MaxFileBytes*4/3 + MaxRequestBytes + 1<<20

// Handler serves the work queue's HTTP API. It submits work to desktops and
// spends model budget, so it belongs on the primary listener only.
type Handler struct {
	Store *Store
	// Dispatcher is woken after an enqueue or retry, and gives spend periods;
	// nil leaves items for the next tick and makes /api/queue/spend answer 503.
	Dispatcher *Dispatcher
}

// Register adds the queue's routes to mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/queue", h.queue)
	mux.HandleFunc("/api/queue/cancel", h.cancel)
	mux.HandleFunc("/api/queue/retry", h.retry)
	mux.HandleFunc("/api/queue/capacity", h.capacity)
	mux.HandleFunc("/api/queue/limits", h.limits)
	mux.HandleFunc("/api/queue/spend", h.spend)
}

// Paths lists the routes Register adds, for wrapping them one by one.
func Paths() []string {
	return []string{"/api/queue", "/api/queue/cancel", "/api/queue/retry", "/api/queue/capacity", "/api/queue/limits", "/api/queue/spend"}
}

// queue lists items (GET) or enqueues one (POST).
func (h *Handler) queue(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		status := q.Get("status")
		if status != "" && !knownStatus(status) {
			writeError(w, http.StatusBadRequest, "unknown status")
			return
		}
		items, err := h.Store.List(Filter{Status: status, Worker: q.Get("worker"), Limit: limit})
		if err != nil {
			storeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	case http.MethodPost:
		var in NewItem
		if !decode(w, r, &in) {
			return
		}
		it, err := h.Store.Enqueue(in)
		if err != nil {
			storeError(w, err)
			return
		}
		h.wake()
		writeJSON(w, http.StatusCreated, it)
	default:
		writeError(w, http.StatusMethodNotAllowed, "use GET or POST")
	}
}

// knownStatus reports whether s is an item status.
func knownStatus(s string) bool {
	switch s {
	case StatusQueued, StatusAssigned, StatusRunning, StatusDone, StatusNotDone, StatusFailed, StatusCancelled, StatusHeld:
		return true
	}
	return false
}

// idRequest is the body of cancel and retry.
type idRequest struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// cancel stops an item.
func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	var in idRequest
	if !post(w, r) || !decode(w, r, &in) {
		return
	}
	it, err := h.Store.Cancel(in.ID, in.Reason)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, it)
}

// retry puts an ended or held item back in the queue.
func (h *Handler) retry(w http.ResponseWriter, r *http.Request) {
	var in idRequest
	if !post(w, r) || !decode(w, r, &in) {
		return
	}
	it, err := h.Store.Retry(in.ID)
	if err != nil {
		storeError(w, err)
		return
	}
	h.wake()
	writeJSON(w, http.StatusOK, it)
}

// capacity lists (GET) or sets (POST) worker capacity.
func (h *Handler) capacity(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		all, err := h.Store.Capacities()
		if err != nil {
			storeError(w, err)
			return
		}
		list := make([]Capacity, 0, len(all))
		for _, c := range all {
			list = append(list, c)
		}
		writeJSON(w, http.StatusOK, map[string]any{"capacity": list, "default_max_concurrent": 1})
	case http.MethodPost:
		var in Capacity
		if !decode(w, r, &in) {
			return
		}
		c, err := h.Store.SetCapacity(in)
		if err != nil {
			storeError(w, err)
			return
		}
		h.wake()
		writeJSON(w, http.StatusOK, c)
	default:
		writeError(w, http.StatusMethodNotAllowed, "use GET or POST")
	}
}

// limits lists (GET) or sets (POST) spend limits; a POST with no cap removes one.
func (h *Handler) limits(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		all, err := h.Store.Limits()
		if err != nil {
			storeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"limits": all, "enforcement": LimitEnforcement})
	case http.MethodPost:
		var in Limit
		if !decode(w, r, &in) {
			return
		}
		l, err := h.Store.SetLimit(in)
		if err != nil {
			storeError(w, err)
			return
		}
		h.wake()
		writeJSON(w, http.StatusOK, l)
	default:
		writeError(w, http.StatusMethodNotAllowed, "use GET or POST")
	}
}

// periodSpend is one period's spend with the limit that applies.
type periodSpend struct {
	Since time.Time `json:"since"`
	Spend
	Limit *Limit `json:"limit,omitempty"`
}

// spend reports a worker's spend this day and this month.
func (h *Handler) spend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	worker := r.URL.Query().Get("worker")
	if !ValidWorker(worker) {
		writeError(w, http.StatusBadRequest, "not a worker id")
		return
	}
	d := h.Dispatcher
	if d == nil || d.Spend == nil {
		writeError(w, http.StatusServiceUnavailable, "spend is not available")
		return
	}
	limits, err := h.Store.Limits()
	if err != nil {
		storeError(w, err)
		return
	}
	out := map[string]any{"worker": worker}
	now := d.now()
	for _, period := range []string{PeriodDay, PeriodMonth} {
		since := d.PeriodStart(period, now)
		s, err := d.Spend.Spend(worker, since)
		if err != nil {
			storeError(w, err)
			return
		}
		ps := periodSpend{Since: since, Spend: s}
		if l, ok := limitFor(limits, worker, period); ok {
			ps.Limit = &l
		}
		out[period] = ps
	}
	writeJSON(w, http.StatusOK, out)
}

// wake nudges the dispatcher, if there is one.
func (h *Handler) wake() {
	if h.Dispatcher != nil {
		h.Dispatcher.Wake()
	}
}

// post refuses anything but POST.
func post(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return false
	}
	return true
}

// decode reads a bounded JSON body into v, answering 400 when it cannot.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

// storeError answers a store error without echoing internal detail.
func storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalid):
		writeError(w, http.StatusBadRequest, Reason(err))
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "no such item")
	case errors.Is(err, ErrConflict):
		writeError(w, http.StatusConflict, "not allowed in the item's current status")
	default:
		log.Printf("workqueue: %v", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
	}
}

// writeJSON writes v as JSON with status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes a curated reason as {"error": reason}.
func writeError(w http.ResponseWriter, status int, reason string) {
	writeJSON(w, status, map[string]string{"error": strings.TrimSpace(reason)})
}
