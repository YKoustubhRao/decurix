// Route tests for cron triggers read in a time zone (B-37): a schedule or a
// DAG's cron takes an IANA zone, answers with it and with its next run in
// that zone's offset, and refuses an unknown zone with a 400 that names it.

package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/dag"
)

func TestCreateScheduleZoneReturnsZoneAndNextFire(t *testing.T) {
	rec := createSchedule(t, "id=bordu&content=file+the+return&cron=0+9+*+*+*&zone=Asia%2FKolkata", http.StatusOK)

	var body struct {
		Zone      string `json:"zone"`
		NextRunAt string `json:"next_run_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}

	if body.Zone != "Asia/Kolkata" {
		t.Fatalf("zone = %q, want Asia/Kolkata", body.Zone)
	}

	next, err := time.Parse(time.RFC3339, body.NextRunAt)
	if err != nil {
		t.Fatalf("next_run_at = %q, not RFC3339: %v", body.NextRunAt, err)
	}

	if !strings.HasSuffix(body.NextRunAt, "T09:00:00+05:30") || !next.After(time.Now()) {
		t.Fatalf("next_run_at = %q, want the next 09:00 in IST with its offset", body.NextRunAt)
	}
}

func TestCreateScheduleRefusesUnknownZone(t *testing.T) {
	rec := createSchedule(t, "id=bordu&content=file+the+return&cron=0+9+*+*+*&zone=Mars%2FOlympus_Mons", http.StatusBadRequest)

	if body := rec.Body.String(); !strings.Contains(body, "unknown time zone") || !strings.Contains(body, "Mars/Olympus_Mons") {
		t.Fatalf("body = %s, want a message naming the unknown zone", body)
	}
}

func TestCreateScheduleWithoutZoneFallsBack(t *testing.T) {
	rec := createSchedule(t, "id=bordu&content=file+the+return&cron=0+9+*+*+*", http.StatusOK)

	var body struct {
		Zone string `json:"zone"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}

	if body.Zone == "" || body.Zone == "Local" {
		t.Fatalf("zone = %q, want the control plane's IANA zone or UTC", body.Zone)
	}
}

func TestSetDagCronZone(t *testing.T) {
	store, err := dag.NewStore(testDB(t))
	if err != nil {
		t.Fatal(err)
	}

	d, err := store.Create("pipeline")
	if err != nil {
		t.Fatal(err)
	}

	s := &Server{Dags: store}

	rec := postForm(t, s.setDagCronHandler, "/api/dags/cron/set", "dag_id="+d.ID+"&cron_expr=30+0+*+*+*&zone=Asia%2FKolkata")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var got struct {
		CronZone  string `json:"cron_zone"`
		NextRunAt string `json:"next_run_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}

	if got.CronZone != "Asia/Kolkata" || !strings.HasSuffix(got.NextRunAt, "T00:30:00+05:30") {
		t.Fatalf("got %+v, want Asia/Kolkata and the next 00:30 IST", got)
	}

	rec = postForm(t, s.setDagCronHandler, "/api/dags/cron/set", "dag_id="+d.ID+"&cron_expr=30+0+*+*+*&zone=Nowhere%2FAtAll")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unknown time zone") {
		t.Fatalf("unknown zone: status = %d, body = %s; want a 400 naming it", rec.Code, rec.Body.String())
	}
}
