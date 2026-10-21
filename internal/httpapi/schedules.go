package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/schedule"
)

// schedulesHandler returns the recurring requests set up from the requests
// page, optionally filtered to one desktop with ?id=.
func (s *Server) schedulesHandler(w http.ResponseWriter, r *http.Request) {
	if !requireGet(w, r) {
		return
	}

	id := r.URL.Query().Get("id")
	if id != "" {
		id = desktop.SanitizeID(id)
	}

	jsonResponse(w, s.Schedules.List(id))
}

// createScheduleHandler sets up a recurring request (cron) or a one-time
// request held for a single future run (run_at, an RFC3339 timestamp) - both
// resubmit the same content through the same path a one-off Send takes (see
// requests.go's submitLocked and internal/schedule's Runner), differing only
// in whether there is a next occurrence after the first.
func (s *Server) createScheduleHandler(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}

	desktopID := desktop.SanitizeID(strings.TrimSpace(r.FormValue("id")))
	content := r.FormValue("content")
	cronExpr := strings.TrimSpace(r.FormValue("cron"))
	runAtStr := strings.TrimSpace(r.FormValue("run_at"))

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

	if _, ok := s.desktopFound(w, r, desktopID, http.StatusBadRequest, "desktop not found"); !ok {
		return
	}

	if cronExpr != "" && runAtStr != "" {
		http.Error(w, "choose either a repeating schedule or a one-time run, not both", http.StatusBadRequest)
		return
	}

	var sch schedule.Schedule
	var err error

	switch {
	case runAtStr != "":
		runAt, perr := time.Parse(time.RFC3339, runAtStr)
		if perr != nil {
			http.Error(w, "invalid run time", http.StatusBadRequest)
			return
		}

		sch, err = s.Schedules.CreateOnce(desktopID, content, runAt)
	case cronExpr != "":
		sch, err = s.Schedules.Create(desktopID, content, cronExpr)
	default:
		http.Error(w, "missing cron expression or run time", http.StatusBadRequest)
		return
	}

	if err != nil {
		jsonStatusError(w, http.StatusBadRequest, err.Error())
		return
	}

	jsonResponse(w, sch)
}

// deleteScheduleHandler removes a schedule by ID.
func (s *Server) deleteScheduleHandler(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}

	s.Schedules.Delete(r.FormValue("id"))

	jsonResponse(w, map[string]string{"status": "deleted"})
}

// toggleScheduleHandler pauses or resumes a schedule without deleting it.
func (s *Server) toggleScheduleHandler(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
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
