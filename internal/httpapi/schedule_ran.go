// The schedule runner's question to the task records after a crash (did this schedule's run reach them?), and a
// schedule's occurrence ledger for the requests page.
package httpapi

import (
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// scheduleRunsHandler returns one schedule's occurrence ledger (?id=), newest first: each run sent, retried, given
// up, unconfirmed after a crash, or missed.
func (s *Server) scheduleRunsHandler(w http.ResponseWriter, r *http.Request) {
	if !requireGet(w, r) {
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		jsonStatusError(w, http.StatusBadRequest, "id is required")
		return
	}
	runs, err := s.Schedules.Runs(id, 100)
	if err != nil {
		log.Printf("schedule runs %s: %v", id, err)
		jsonStatusError(w, http.StatusInternalServerError, "could not read the schedule's runs")
		return
	}
	jsonResponse(w, runs)
}

// ScheduleRanSince implements internal/schedule's RanChecker: whether an execution the schedule started was opened
// at or after since. With no task records it cannot say, and says so, so the occurrence is not sent a second time.
func (s *Server) ScheduleRanSince(scheduleID string, since time.Time) (bool, error) {
	if s.Tasks == nil {
		return false, errors.New("no task records")
	}
	return s.Tasks.RanFromOrigin(tasks.OriginSchedule, scheduleID, since)
}
