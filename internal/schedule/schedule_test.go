package schedule

import (
	"testing"
	"time"

	"github.com/YKoustubhRao/flowera/internal/sqlitedb"
)

func newTestStore(t *testing.T, now time.Time) *Store {
	t.Helper()

	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}

	store.now = func() time.Time { return now }

	return store
}

func TestCreateRejectsBadCron(t *testing.T) {
	store := newTestStore(t, time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))

	if _, err := store.Create("bordu", "check the mail", "not a cron"); err == nil {
		t.Fatal("expected an error for an invalid cron expression")
	}
}

func TestCreateSchedulesTheNextOccurrenceNotNow(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 30, 0, 0, time.UTC)
	store := newTestStore(t, now)

	sch, err := store.Create("bordu", "check the mail", "*/10 * * * *")
	if err != nil {
		t.Fatal(err)
	}

	want := time.Date(2026, 9, 15, 12, 40, 0, 0, time.UTC)
	if !sch.NextRunAt.Equal(want) {
		t.Fatalf("NextRunAt = %v, want %v", sch.NextRunAt, want)
	}

	if !sch.LastRunAt.IsZero() {
		t.Fatalf("a freshly created schedule should not have a last run, got %v", sch.LastRunAt)
	}

	due, err := store.Due(now)
	if err != nil {
		t.Fatal(err)
	}

	if len(due) != 0 {
		t.Fatalf("a schedule should not be due the moment it is created, got %d due", len(due))
	}
}

func TestDueAndMarkRunAdvances(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 30, 0, 0, time.UTC)
	store := newTestStore(t, now)

	sch, err := store.Create("bordu", "check the mail", "*/10 * * * *")
	if err != nil {
		t.Fatal(err)
	}

	firedAt := time.Date(2026, 9, 15, 12, 40, 0, 0, time.UTC)

	due, err := store.Due(firedAt)
	if err != nil {
		t.Fatal(err)
	}

	if len(due) != 1 || due[0].ID != sch.ID {
		t.Fatalf("Due = %v, want [%s]", due, sch.ID)
	}

	if err := store.MarkRun(sch.ID, firedAt); err != nil {
		t.Fatal(err)
	}

	after := store.List("")
	if len(after) != 1 {
		t.Fatalf("List = %v", after)
	}

	if !after[0].LastRunAt.Equal(firedAt) {
		t.Fatalf("LastRunAt = %v, want %v", after[0].LastRunAt, firedAt)
	}

	want := time.Date(2026, 9, 15, 12, 50, 0, 0, time.UTC)
	if !after[0].NextRunAt.Equal(want) {
		t.Fatalf("NextRunAt = %v, want %v", after[0].NextRunAt, want)
	}

	// Not due again immediately after firing.
	due, err = store.Due(firedAt)
	if err != nil {
		t.Fatal(err)
	}

	if len(due) != 0 {
		t.Fatalf("expected nothing due right after firing, got %v", due)
	}
}

func TestMarkRunOfImpossibleCronDisablesTheSchedule(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := newTestStore(t, now)

	sch, err := store.Create("bordu", "check the mail", "0 0 * * *")
	if err != nil {
		t.Fatal(err)
	}

	// Corrupt the stored expression to one that can never match again, the
	// way an edited/broken cron_expr in the database would, and confirm
	// MarkRun disables rather than looping on it.
	if _, err := store.db.Exec(`UPDATE schedules SET cron_expr = '0 0 31 4 *' WHERE id = ?`, sch.ID); err != nil {
		t.Fatal(err)
	}

	if err := store.MarkRun(sch.ID, now); err != nil {
		t.Fatal(err)
	}

	after := store.List("")
	if len(after) != 1 {
		t.Fatalf("List = %v", after)
	}

	if after[0].Enabled {
		t.Fatal("expected the schedule to be disabled after an impossible run")
	}
}

func TestSetEnabledRecomputesNextRun(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 30, 0, 0, time.UTC)
	store := newTestStore(t, now)

	sch, err := store.Create("bordu", "check the mail", "0 0 * * *")
	if err != nil {
		t.Fatal(err)
	}

	if err := store.SetEnabled(sch.ID, false); err != nil {
		t.Fatal(err)
	}

	// Time passes while the schedule sits disabled.
	later := now.Add(72 * time.Hour)
	store.now = func() time.Time { return later }

	if err := store.SetEnabled(sch.ID, true); err != nil {
		t.Fatal(err)
	}

	after := store.List("")
	if len(after) != 1 {
		t.Fatalf("List = %v", after)
	}

	if !after[0].Enabled {
		t.Fatal("expected the schedule to be enabled again")
	}

	if !after[0].NextRunAt.After(later) {
		t.Fatalf("NextRunAt = %v, want something after %v (recomputed from now, not from before it was disabled)", after[0].NextRunAt, later)
	}
}

func TestDeleteRemovesSchedule(t *testing.T) {
	store := newTestStore(t, time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))

	sch, err := store.Create("bordu", "check the mail", "* * * * *")
	if err != nil {
		t.Fatal(err)
	}

	store.Delete(sch.ID)

	if got := store.List(""); len(got) != 0 {
		t.Fatalf("List after delete = %v, want empty", got)
	}
}

func TestListFiltersByDesktop(t *testing.T) {
	store := newTestStore(t, time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))

	if _, err := store.Create("bordu", "check the mail", "* * * * *"); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Create("tanager", "check the mail", "* * * * *"); err != nil {
		t.Fatal(err)
	}

	if got := store.List("bordu"); len(got) != 1 || got[0].DesktopID != "bordu" {
		t.Fatalf("List(bordu) = %v", got)
	}

	if got := store.List(""); len(got) != 2 {
		t.Fatalf("List(\"\") = %v, want 2", got)
	}
}
