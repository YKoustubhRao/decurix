package schedule

import (
	"path/filepath"
	"sync"
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

func TestCreateOnceRejectsAPastTime(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	store := newTestStore(t, now)

	if _, err := store.CreateOnce("bordu", "check the mail", now.Add(-time.Minute)); err == nil {
		t.Fatal("expected an error for a run time in the past")
	}

	if _, err := store.CreateOnce("bordu", "check the mail", now); err == nil {
		t.Fatal("expected an error for a run time equal to now")
	}
}

func TestCreateOnceAndMarkRunDisablesRatherThanRecurring(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	store := newTestStore(t, now)

	runAt := now.Add(time.Hour)

	sch, err := store.CreateOnce("bordu", "check the mail", runAt)
	if err != nil {
		t.Fatal(err)
	}

	if !sch.RunOnce {
		t.Fatal("expected RunOnce to be true")
	}

	if !sch.NextRunAt.Equal(runAt) {
		t.Fatalf("NextRunAt = %v, want %v", sch.NextRunAt, runAt)
	}

	due, err := store.Due(runAt)
	if err != nil {
		t.Fatal(err)
	}

	if len(due) != 1 || due[0].ID != sch.ID {
		t.Fatalf("Due = %v, want [%s]", due, sch.ID)
	}

	if err := store.MarkRun(sch.ID, runAt); err != nil {
		t.Fatal(err)
	}

	after := store.List("")
	if len(after) != 1 {
		t.Fatalf("List = %v", after)
	}

	if after[0].Enabled {
		t.Fatal("expected a one-time schedule to disable itself after firing")
	}

	if !after[0].LastRunAt.Equal(runAt) {
		t.Fatalf("LastRunAt = %v, want %v", after[0].LastRunAt, runAt)
	}

	if !after[0].NextRunAt.IsZero() {
		t.Fatalf("expected no next run after a one-time schedule fires, got %v", after[0].NextRunAt)
	}

	// Never due again.
	due, err = store.Due(runAt.Add(24 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	if len(due) != 0 {
		t.Fatalf("expected a fired one-time schedule never to be due again, got %v", due)
	}
}

func TestSetEnabledOnAFiredOnceScheduleErrors(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	store := newTestStore(t, now)

	runAt := now.Add(time.Hour)

	sch, err := store.CreateOnce("bordu", "check the mail", runAt)
	if err != nil {
		t.Fatal(err)
	}

	if err := store.MarkRun(sch.ID, runAt); err != nil {
		t.Fatal(err)
	}

	if err := store.SetEnabled(sch.ID, true); err == nil {
		t.Fatal("expected an error resuming a one-time schedule that already fired")
	}
}

func TestSetEnabledOnAPendingOnceScheduleKeepsItsRunTime(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	store := newTestStore(t, now)

	runAt := now.Add(time.Hour)

	sch, err := store.CreateOnce("bordu", "check the mail", runAt)
	if err != nil {
		t.Fatal(err)
	}

	if err := store.SetEnabled(sch.ID, false); err != nil {
		t.Fatal(err)
	}

	// Time passes while paused, past the original run time.
	later := runAt.Add(time.Hour)
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

	if !after[0].NextRunAt.Equal(runAt) {
		t.Fatalf("NextRunAt = %v, want the original %v preserved, not recomputed", after[0].NextRunAt, runAt)
	}

	// Now due as a catch-up run, same as a cron schedule due after an outage.
	due, err := store.Due(later)
	if err != nil {
		t.Fatal(err)
	}

	if len(due) != 1 || due[0].ID != sch.ID {
		t.Fatalf("Due = %v, want [%s]", due, sch.ID)
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

func TestDeleteForDesktopRemovesOnlyThatDesktopsSchedules(t *testing.T) {
	store := newTestStore(t, time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))

	if _, err := store.Create("bordu", "check the mail", "* * * * *"); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Create("bordu", "water the plants", "0 9 * * *"); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Create("tanager", "check the mail", "* * * * *"); err != nil {
		t.Fatal(err)
	}

	store.DeleteForDesktop("bordu")

	if got := store.List("bordu"); len(got) != 0 {
		t.Fatalf("List(bordu) after DeleteForDesktop = %v, want empty", got)
	}

	if got := store.List("tanager"); len(got) != 1 {
		t.Fatalf("List(tanager) after DeleteForDesktop(bordu) = %v, want 1 untouched schedule", got)
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

// TestConcurrentMarkRunAndSetEnabledDoNotLoseAnUpdate guards the race
// Store.mu closes: MarkRun and SetEnabled each read a schedule's row and
// write it back, and sqlitedb's pool is no longer one connection serialising
// every call for them. It needs a real, file-backed store (sqlitedb.OpenMemory
// stays pinned to one connection deliberately, which would serialise these
// calls anyway and hide the bug being guarded against) so the calls can
// actually run on different connections at once.
//
// A RunOnce schedule gives a crisp, deterministic invariant to check:
// SetEnabled's own doc comment says a schedule that has already fired is a
// terminal state MarkRun never returns from - enabled=1 and a non-zero
// LastRunAt must never both be true at once. Without the mutex, this
// interleaving loses an update: SetEnabled reads last_run_ns=0 (not fired
// yet), MarkRun fires in between and sets enabled=0 with last_run_ns=now,
// then SetEnabled's write - working off what it already read - sets
// enabled=1 back onto a schedule that has, by then, already fired.
func TestConcurrentMarkRunAndSetEnabledDoNotLoseAnUpdate(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	runAt := now.Add(time.Hour)

	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "flowera.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return now }

	sch, err := store.CreateOnce("bordu", "check the mail", runAt)
	if err != nil {
		t.Fatal(err)
	}

	const iterations = 500

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if err := store.MarkRun(sch.ID, now); err != nil {
				t.Error(err)
			}
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			// Errors are expected once MarkRun's disable has landed -
			// SetEnabled correctly refuses a fired RunOnce schedule. What
			// must never happen is a write slipping through anyway.
			store.SetEnabled(sch.ID, true)
		}
	}()

	wg.Wait()

	got := store.List("bordu")
	if len(got) != 1 {
		t.Fatalf("List = %v, want exactly the one schedule", got)
	}

	final := got[0]
	if final.Enabled && !final.LastRunAt.IsZero() {
		t.Fatalf("lost update: schedule re-enabled after it had already fired: %+v", final)
	}
}
