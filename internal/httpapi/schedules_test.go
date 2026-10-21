package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/schedule"
)

// scheduleServer builds the slice of Server createScheduleHandler and its
// siblings actually use - just the schedule store, the same way chatServer
// scopes itself to what the chat handlers touch.
func scheduleServer(t *testing.T) *Server {
	t.Helper()

	db := testDB(t)

	store, err := schedule.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}

	return &Server{Schedules: store, Desktops: fakeDesktopManager(t, "bordu")}
}

// createSchedule posts form to a fresh scheduleServer's createScheduleHandler
// and fails the test unless it answers want.
func createSchedule(t *testing.T, form string, want int) *httptest.ResponseRecorder {
	t.Helper()

	rec := postForm(t, scheduleServer(t).createScheduleHandler, "/api/schedules/create", form)

	if rec.Code != want {
		t.Fatalf("status = %d, want %d, body = %s", rec.Code, want, rec.Body.String())
	}

	return rec
}

func TestCreateScheduleHandlerRunAtCreatesAOneTimeSchedule(t *testing.T) {
	runAt := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)

	rec := createSchedule(t, "id=bordu&content=check+the+mail&run_at="+runAt, http.StatusOK)

	var sch schedule.Schedule
	if err := json.Unmarshal(rec.Body.Bytes(), &sch); err != nil {
		t.Fatal(err)
	}

	if !sch.RunOnce {
		t.Error("expected the created schedule to be RunOnce")
	}

	if sch.CronExpr != "" {
		t.Errorf("cron_expr = %q, want empty for a one-time run", sch.CronExpr)
	}
}

func TestCreateScheduleHandlerRejectsBothCronAndRunAt(t *testing.T) {
	runAt := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)

	createSchedule(t, "id=bordu&content=check+the+mail&cron=0+9+*+*+*&run_at="+runAt, http.StatusBadRequest)
}

func TestCreateScheduleHandlerRejectsMalformedRunAt(t *testing.T) {
	createSchedule(t, "id=bordu&content=check+the+mail&run_at=not-a-time", http.StatusBadRequest)
}

func TestCreateScheduleHandlerRejectsPastRunAt(t *testing.T) {
	runAt := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)

	createSchedule(t, "id=bordu&content=check+the+mail&run_at="+runAt, http.StatusBadRequest)
}

func TestCreateScheduleHandlerRequiresCronOrRunAt(t *testing.T) {
	createSchedule(t, "id=bordu&content=check+the+mail", http.StatusBadRequest)
}

func TestCreateScheduleHandlerStillCreatesRecurringSchedules(t *testing.T) {
	rec := createSchedule(t, "id=bordu&content=check+the+mail&cron=0+9+*+*+*", http.StatusOK)

	var sch schedule.Schedule
	if err := json.Unmarshal(rec.Body.Bytes(), &sch); err != nil {
		t.Fatal(err)
	}

	if sch.RunOnce {
		t.Error("expected a cron schedule not to be RunOnce")
	}

	if sch.CronExpr != "0 9 * * *" {
		t.Errorf("cron_expr = %q", sch.CronExpr)
	}
}
