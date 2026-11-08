// Tests for cron schedules read in a time zone (B-37): matching on a zone's
// wall clock, a half-hour offset, both daylight-saving changes, schedules
// made before zones existed keeping UTC, and unknown zone names refused.

package schedule

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// mustZone loads an IANA zone or fails the test.
func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()

	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}

	return loc
}

// nextChain returns the first n fire times of spec in loc strictly after after.
func nextChain(t *testing.T, spec string, loc *time.Location, after time.Time, n int) []time.Time {
	t.Helper()

	expr := mustParse(t, spec)
	out := make([]time.Time, 0, n)

	for i := 0; i < n; i++ {
		next, ok := expr.NextIn(after, loc)
		if !ok {
			t.Fatalf("%q in %s: no match after %v", spec, loc, after)
		}

		out = append(out, next)
		after = next
	}

	return out
}

// wantInstants fails unless got equals want, instant by instant, and every
// got is given in loc.
func wantInstants(t *testing.T, got []time.Time, loc *time.Location, want ...time.Time) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("got %d times, want %d", len(got), len(want))
	}

	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Errorf("fire %d = %v (%v UTC), want %v (%v UTC)", i, got[i], got[i].UTC(), want[i].In(loc), want[i].UTC())
		}

		if got[i].Location() != loc {
			t.Errorf("fire %d is in %v, want it given in %v", i, got[i].Location(), loc)
		}
	}
}

func TestNextInKolkataHalfHourOffset(t *testing.T) {
	ist := mustZone(t, "Asia/Kolkata")

	// 09:00 IST is 03:30 UTC.
	got := nextChain(t, "0 9 * * *", ist, time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC), 2)
	wantInstants(t, got, ist,
		time.Date(2026, 11, 7, 3, 30, 0, 0, time.UTC),
		time.Date(2026, 11, 8, 3, 30, 0, 0, time.UTC),
	)

	if s := got[0].Format(time.RFC3339); s != "2026-11-07T09:00:00+05:30" {
		t.Errorf("RFC3339 = %q, want the IST wall clock with its offset", s)
	}

	// 00:30 IST on 8 Nov is still 7 Nov in UTC: the day is read in IST.
	got = nextChain(t, "30 0 8 * *", ist, time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC), 1)
	wantInstants(t, got, ist, time.Date(2026, 11, 7, 19, 0, 0, 0, time.UTC))

	// Monday 01:00 IST is Sunday 19:30 UTC: the weekday is read in IST too.
	got = nextChain(t, "0 1 * * 1", ist, time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC), 1)
	wantInstants(t, got, ist, time.Date(2026, 11, 8, 19, 30, 0, 0, time.UTC))
}

func TestNextIsUTC(t *testing.T) {
	next, ok := mustParse(t, "0 9 * * *").Next(time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC))
	if !ok || !next.Equal(time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)) || next.Location() != time.UTC {
		t.Fatalf("Next = %v, %v; want 09:00 UTC", next, ok)
	}
}

func TestNextInSpringForwardFixedTimeRunsAfterTheJump(t *testing.T) {
	ny := mustZone(t, "America/New_York")

	// 14 Mar 2027: 02:00 EST jumps to 03:00 EDT, so 02:30 never happens.
	// A daily 02:30 runs at 03:00 EDT that day, then at 02:30 again.
	got := nextChain(t, "30 2 * * *", ny, time.Date(2027, 3, 13, 12, 0, 0, 0, ny), 3)
	wantInstants(t, got, ny,
		time.Date(2027, 3, 14, 7, 0, 0, 0, time.UTC),  // 03:00 EDT
		time.Date(2027, 3, 15, 6, 30, 0, 0, time.UTC), // 02:30 EDT
		time.Date(2027, 3, 16, 6, 30, 0, 0, time.UTC),
	)
}

func TestNextInSpringForwardWildcardFollowsTheClock(t *testing.T) {
	ny := mustZone(t, "America/New_York")

	// Every 30 minutes: 01:30 EST, then 03:00 EDT - no runs for the minutes
	// that never happen, and no extra one at the jump.
	got := nextChain(t, "*/30 * * * *", ny, time.Date(2027, 3, 14, 1, 15, 0, 0, ny), 3)
	wantInstants(t, got, ny,
		time.Date(2027, 3, 14, 6, 30, 0, 0, time.UTC), // 01:30 EST
		time.Date(2027, 3, 14, 7, 0, 0, 0, time.UTC),  // 03:00 EDT
		time.Date(2027, 3, 14, 7, 30, 0, 0, time.UTC), // 03:30 EDT
	)
}

func TestNextInFallBackFixedTimeRunsOnce(t *testing.T) {
	ny := mustZone(t, "America/New_York")

	// 7 Nov 2027: 02:00 EDT goes back to 01:00 EST, so 01:30 happens twice.
	// A daily 01:30 runs at the first of them only.
	got := nextChain(t, "30 1 * * *", ny, time.Date(2027, 11, 7, 0, 0, 0, 0, ny), 2)
	wantInstants(t, got, ny,
		time.Date(2027, 11, 7, 5, 30, 0, 0, time.UTC), // 01:30 EDT
		time.Date(2027, 11, 8, 6, 30, 0, 0, time.UTC), // 01:30 EST, the next day
	)
}

func TestNextInFallBackWildcardRunsEachRealMinute(t *testing.T) {
	ny := mustZone(t, "America/New_York")

	got := nextChain(t, "*/30 1 * * *", ny, time.Date(2027, 11, 7, 0, 45, 0, 0, ny), 4)
	wantInstants(t, got, ny,
		time.Date(2027, 11, 7, 5, 0, 0, 0, time.UTC),  // 01:00 EDT
		time.Date(2027, 11, 7, 5, 30, 0, 0, time.UTC), // 01:30 EDT
		time.Date(2027, 11, 7, 6, 0, 0, 0, time.UTC),  // 01:00 EST
		time.Date(2027, 11, 7, 6, 30, 0, 0, time.UTC), // 01:30 EST
	)
}

func TestLoadZoneRefusesUnknownNames(t *testing.T) {
	for _, name := range []string{"Mars/Olympus_Mons", "Local", "../etc/passwd", strings.Repeat("A", 100)} {
		_, _, err := LoadZone(name)

		var input InputError
		if !errors.As(err, &input) {
			t.Errorf("LoadZone(%q) = %v, want an InputError", name, err)
		}
	}

	name, loc, err := LoadZone(" Asia/Kolkata ")
	if err != nil || name != "Asia/Kolkata" || loc.String() != "Asia/Kolkata" {
		t.Fatalf("LoadZone(Asia/Kolkata) = %q, %v, %v", name, loc, err)
	}

	if name, _, err := LoadZone(""); err != nil || name == "" || name == "Local" {
		t.Fatalf("LoadZone(\"\") = %q, %v; want the default zone's IANA name", name, err)
	}
}

func TestCreateInZoneStoresZoneAndNextRun(t *testing.T) {
	store := newTestStore(t, time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC))

	sch, err := store.Create("bordu", "file the return", "0 9 * * *", "Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}

	want := time.Date(2026, 11, 7, 3, 30, 0, 0, time.UTC)
	if sch.Zone != "Asia/Kolkata" || !sch.NextRunAt.Equal(want) {
		t.Fatalf("created %q next %v, want Asia/Kolkata next %v", sch.Zone, sch.NextRunAt, want)
	}

	listed := store.List("bordu")
	if len(listed) != 1 || listed[0].Zone != "Asia/Kolkata" {
		t.Fatalf("List = %+v", listed)
	}

	if s := listed[0].NextRunAt.Format(time.RFC3339); s != "2026-11-07T09:00:00+05:30" {
		t.Fatalf("listed next run = %q, want it given in the schedule's zone", s)
	}

	// After the run, the next is 09:00 IST the next day, not 09:00 UTC.
	if err := store.MarkRun(sch.ID, want); err != nil {
		t.Fatal(err)
	}

	if got := store.List("bordu")[0].NextRunAt; !got.Equal(want.Add(24 * time.Hour)) {
		t.Fatalf("next after run = %v, want %v", got, want.Add(24*time.Hour))
	}

	if _, err := store.Create("bordu", "file the return", "0 9 * * *", "Mars/Olympus_Mons"); err == nil {
		t.Fatal("expected an unknown zone to be refused")
	}
}

func TestRunnerTimeFieldIsInTheSchedulesZone(t *testing.T) {
	store := newTestStore(t, time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC))

	if _, err := store.Create("bordu", "daily close", "30 0 * * *", "Asia/Kolkata"); err != nil {
		t.Fatal(err)
	}

	submitter := &fakeSubmitter{}
	firedAt := time.Date(2026, 11, 7, 19, 0, 0, 0, time.UTC) // 00:30 IST on the 8th
	runner := &Runner{Store: store, Submitter: submitter, now: func() time.Time { return firedAt }}

	runner.tick(context.Background())

	submitter.mu.Lock()
	defer submitter.mu.Unlock()

	if len(submitter.calls) != 1 || !strings.HasSuffix(submitter.calls[0].content, "TIME:{2026-11-08T00:30:00+05:30}") {
		t.Fatalf("calls = %+v, want one whose TIME is the IST date and time", submitter.calls)
	}
}

func TestSchedulesFromBeforeZonesKeepUTC(t *testing.T) {
	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	// The schema as it stood before the zone step, and a schedule made then.
	if err := sqlitedb.Migrate(db, "schedule", schema[:len(schema)-1]); err != nil {
		t.Fatal(err)
	}

	next := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	if _, err := db.Exec(
		`INSERT INTO schedules (id, desktop_id, content, cron_expr, enabled, created_ns, next_run_ns) VALUES ('sched-old', 'bordu', 'old one', '0 9 * * *', 1, 1, ?)`,
		sqlitedb.ToNS(next),
	); err != nil {
		t.Fatal(err)
	}

	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}

	got := store.List("bordu")
	if len(got) != 1 || got[0].Zone != "UTC" || !got[0].NextRunAt.Equal(next) {
		t.Fatalf("migrated = %+v, want zone UTC and the same next run", got)
	}

	if err := store.MarkRun("sched-old", next); err != nil {
		t.Fatal(err)
	}

	if after := store.List("bordu")[0].NextRunAt; !after.Equal(next.Add(24 * time.Hour)) {
		t.Fatalf("next after run = %v, want 09:00 UTC the next day", after)
	}
}
