package schedule

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, spec string) Expr {
	t.Helper()

	expr, err := Parse(spec)
	if err != nil {
		t.Fatalf("Parse(%q): %v", spec, err)
	}

	return expr
}

func TestParseRejectsWrongFieldCount(t *testing.T) {
	if _, err := Parse("* * * *"); err == nil {
		t.Fatal("expected an error for a 4-field expression")
	}
}

func TestParseRejectsOutOfRangeValue(t *testing.T) {
	if _, err := Parse("60 * * * *"); err == nil {
		t.Fatal("expected an error for minute 60")
	}
}

// assertNext checks that spec's first match after after is want.
func assertNext(t *testing.T, spec string, after, want time.Time) {
	t.Helper()

	next, ok := mustParse(t, spec).Next(after)
	if !ok {
		t.Fatal("expected a match")
	}

	if !next.Equal(want) {
		t.Fatalf("Next = %v, want %v", next, want)
	}
}

func TestEveryMinute(t *testing.T) {
	assertNext(t, "* * * * *", time.Date(2026, 9, 15, 12, 30, 0, 0, time.UTC), time.Date(2026, 9, 15, 12, 31, 0, 0, time.UTC))
}

func TestEveryFiveMinutesStep(t *testing.T) {
	assertNext(t, "*/5 * * * *", time.Date(2026, 9, 15, 12, 31, 0, 0, time.UTC), time.Date(2026, 9, 15, 12, 35, 0, 0, time.UTC))
}

func TestDailyAtSpecificTime(t *testing.T) {
	assertNext(t, "30 9 * * *", time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC), time.Date(2026, 9, 16, 9, 30, 0, 0, time.UTC))
}

// A weekly schedule (day-of-week restricted, day-of-month left as "*") only
// needs the day-of-week to match.
func TestWeeklyOnWeekday(t *testing.T) {
	expr := mustParse(t, "0 9 * * 1") // every Monday at 09:00

	after := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC) // a Tuesday

	next, ok := expr.Next(after)
	if !ok {
		t.Fatal("expected a match")
	}

	if next.Weekday() != time.Monday || next.Hour() != 9 || next.Minute() != 0 {
		t.Fatalf("Next = %v, want the following Monday at 09:00 UTC", next)
	}
}

// Cron's day-of-month/day-of-week OR rule: when both are restricted, a match
// needs only one of the two, not both.
func TestDayOfMonthOrDayOfWeek(t *testing.T) {
	expr := mustParse(t, "0 0 1 * 1") // midnight on the 1st, OR any Monday

	after := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC) // a Tuesday, not the 1st

	next, ok := expr.Next(after)
	if !ok {
		t.Fatal("expected a match")
	}

	// The next Monday (Sep 21) arrives before the next 1st-of-month
	// midnight (Oct 1), so the OR rule should pick the Monday.
	want := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("Next = %v, want %v (the OR rule should have matched the Monday first)", next, want)
	}
}

func TestImpossibleExpressionNeverMatches(t *testing.T) {
	// April never has a 31st day.
	expr := mustParse(t, "0 0 31 4 *")

	after := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	if _, ok := expr.Next(after); ok {
		t.Fatal("expected no match for April 31st")
	}
}
