// Tests for a DAG's cron trigger read in a time zone (B-37): the zone is
// kept and used for every next run, an unknown zone is refused as the
// person's mistake, and DAGs whose cron was set before zones existed keep
// UTC.

package dag

import (
	"errors"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

func TestSetCronInZone(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	store := newTestStore(t, now)
	d, _ := store.Create("pipeline")

	got, err := store.SetCron(d.ID, "0 9 * * *", "Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}

	want := time.Date(2026, 11, 7, 3, 30, 0, 0, time.UTC) // 09:00 IST
	if got.CronZone != "Asia/Kolkata" || !got.NextRunAt.Equal(want) {
		t.Fatalf("SetCron = %q next %v, want Asia/Kolkata next %v", got.CronZone, got.NextRunAt, want)
	}

	if s := got.NextRunAt.Format(time.RFC3339); s != "2026-11-07T09:00:00+05:30" {
		t.Fatalf("next run = %q, want it given in the cron's zone", s)
	}

	if err := store.MarkCronRun(d.ID, want); err != nil {
		t.Fatal(err)
	}

	after, _ := store.Get(d.ID)
	if !after.NextRunAt.Equal(want.Add(24 * time.Hour)) {
		t.Fatalf("next after run = %v, want 09:00 IST the next day", after.NextRunAt)
	}
}

func TestSetCronRefusesUnknownZone(t *testing.T) {
	store := newTestStore(t, time.Now())
	d, _ := store.Create("pipeline")

	_, err := store.SetCron(d.ID, "0 9 * * *", "Mars/Olympus_Mons")

	var refused *InputError
	if !errors.As(err, &refused) {
		t.Fatalf("SetCron with an unknown zone = %v, want an InputError", err)
	}
}

func TestDagCronFromBeforeZonesKeepsUTC(t *testing.T) {
	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	// The schema as it stood before the zone step, and a cron set then.
	if err := sqlitedb.Migrate(db, "dag", schema[:len(schema)-1]); err != nil {
		t.Fatal(err)
	}

	next := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	if _, err := db.Exec(
		`INSERT INTO dags (id, name, cron_expr, next_run_ns, created_ns, updated_ns) VALUES ('dag-old', 'old', '0 9 * * *', ?, 1, 1)`,
		sqlitedb.ToNS(next),
	); err != nil {
		t.Fatal(err)
	}

	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}

	got, err := store.Get("dag-old")
	if err != nil || got.CronZone != "UTC" || !got.NextRunAt.Equal(next) {
		t.Fatalf("migrated = %+v, %v; want zone UTC and the same next run", got, err)
	}

	if err := store.MarkCronRun("dag-old", next); err != nil {
		t.Fatal(err)
	}

	if after, _ := store.Get("dag-old"); !after.NextRunAt.Equal(next.Add(24 * time.Hour)) {
		t.Fatalf("next after run = %v, want 09:00 UTC the next day", after.NextRunAt)
	}
}
