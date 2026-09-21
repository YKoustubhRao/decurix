package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/schedule"
	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// scheduleServer builds the slice of Server createScheduleHandler and its
// siblings actually use - just the schedule store, the same way chatServer
// scopes itself to what the chat handlers touch.
func scheduleServer(t *testing.T) *Server {
	t.Helper()

	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	store, err := schedule.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}

	return &Server{Schedules: store, Desktops: fakeDesktopManager(t, "bordu")}
}

func TestCreateScheduleHandlerRunAtCreatesAOneTimeSchedule(t *testing.T) {
	s := scheduleServer(t)

	runAt := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)

	rec := postForm(t, s.createScheduleHandler, "/api/schedules/create",
		"id=bordu&content=check+the+mail&run_at="+runAt)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

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
	s := scheduleServer(t)

	runAt := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)

	rec := postForm(t, s.createScheduleHandler, "/api/schedules/create",
		"id=bordu&content=check+the+mail&cron=0+9+*+*+*&run_at="+runAt)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
}

func TestCreateScheduleHandlerRejectsMalformedRunAt(t *testing.T) {
	s := scheduleServer(t)

	rec := postForm(t, s.createScheduleHandler, "/api/schedules/create",
		"id=bordu&content=check+the+mail&run_at=not-a-time")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
}

func TestCreateScheduleHandlerRejectsPastRunAt(t *testing.T) {
	s := scheduleServer(t)

	runAt := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)

	rec := postForm(t, s.createScheduleHandler, "/api/schedules/create",
		"id=bordu&content=check+the+mail&run_at="+runAt)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
}

func TestCreateScheduleHandlerRequiresCronOrRunAt(t *testing.T) {
	s := scheduleServer(t)

	rec := postForm(t, s.createScheduleHandler, "/api/schedules/create", "id=bordu&content=check+the+mail")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
}

func TestCreateScheduleHandlerStillCreatesRecurringSchedules(t *testing.T) {
	s := scheduleServer(t)

	rec := postForm(t, s.createScheduleHandler, "/api/schedules/create",
		"id=bordu&content=check+the+mail&cron=0+9+*+*+*")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

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
