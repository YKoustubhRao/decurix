package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/YKoustubhRao/flowera/internal/desktop"
)

// schedulesHandler returns the recurring requests set up from the requests
// page, optionally filtered to one desktop with ?id=.
func (s *Server) schedulesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}

	id := r.URL.Query().Get("id")
	if id != "" {
		id = desktop.SanitizeID(id)
	}

	jsonResponse(w, s.Schedules.List(id))
}

// createScheduleHandler sets up a recurring request: the same content
// resubmitted to a desktop every time cron next matches, through the same
// path a one-off Send takes (see requests.go's submitRequest and
// internal/schedule's Runner).
func (s *Server) createScheduleHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	desktopID := desktop.SanitizeID(strings.TrimSpace(r.FormValue("id")))
	content := r.FormValue("content")
	cronExpr := strings.TrimSpace(r.FormValue("cron"))

	if desktopID == "" {
		http.Error(w, "missing desktop id", http.StatusBadRequest)
		return
	}

	if content == "" {
		http.Error(w, "empty request", http.StatusBadRequest)
		return
	}

	if len(content) > maxRequestBytes {
		http.Error(w, "request too long", http.StatusBadRequest)
		return
	}

	sch, err := s.Schedules.Create(desktopID, content, cronExpr)
	if err != nil {
		jsonStatusError(w, http.StatusBadRequest, err.Error())
		return
	}

	jsonResponse(w, sch)
}

// deleteScheduleHandler removes a schedule by ID.
func (s *Server) deleteScheduleHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	s.Schedules.Delete(r.FormValue("id"))

	jsonResponse(w, map[string]string{"status": "deleted"})
}

// toggleScheduleHandler pauses or resumes a schedule without deleting it.
func (s *Server) toggleScheduleHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	id := r.FormValue("id")

	enabled, err := strconv.ParseBool(r.FormValue("enabled"))
	if err != nil {
		http.Error(w, "enabled must be true or false", http.StatusBadRequest)
		return
	}

	if err := s.Schedules.SetEnabled(id, enabled); err != nil {
		jsonStatusError(w, http.StatusBadRequest, err.Error())
		return
	}

	jsonResponse(w, map[string]string{"status": "ok"})
}
