package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Expr is a parsed standard 5-field cron expression: minute hour
// day-of-month month day-of-week. It is matched against the wall clock of
// the zone NextIn is given (Next is UTC), never the host's own zone, so a
// schedule fires at the same instant wherever the control plane runs.
type Expr struct {
	minute, hour, dom, month, dow map[int]bool
	minuteAny, hourAny            bool
	domAny, dowAny                bool
}

// Parse parses a standard 5-field cron expression (minute hour day-of-month
// month day-of-week). Each field is a comma-separated list of "*", "N",
// "A-B", "*/N", "A-B/N" or "N/S" (from N to the field's end, every S: vixie
// cron's reading, so "5/15" in the minute field is 5, 20, 35 and 50). A
// weekday is 0-7, with both 0 and 7 meaning Sunday. Names (MON, JAN) are not
// accepted.
func Parse(spec string) (Expr, error) {
	fields := strings.Fields(spec)
	if len(fields) != 5 {
		return Expr{}, fmt.Errorf("cron expression needs 5 fields (minute hour day month weekday), got %d", len(fields))
	}

	minute, minuteAny, err := parseField(fields[0], 0, 59)
	if err != nil {
		return Expr{}, fmt.Errorf("minute field: %w", err)
	}

	hour, hourAny, err := parseField(fields[1], 0, 23)
	if err != nil {
		return Expr{}, fmt.Errorf("hour field: %w", err)
	}

	dom, domAny, err := parseField(fields[2], 1, 31)
	if err != nil {
		return Expr{}, fmt.Errorf("day-of-month field: %w", err)
	}

	month, _, err := parseField(fields[3], 1, 12)
	if err != nil {
		return Expr{}, fmt.Errorf("month field: %w", err)
	}

	dow, dowAny, err := parseField(fields[4], 0, 7)
	if err != nil {
		return Expr{}, fmt.Errorf("day-of-week field: %w", err)
	}

	if dow[7] {
		delete(dow, 7)
		dow[0] = true
	}

	return Expr{
		minute:    minute,
		hour:      hour,
		dom:       dom,
		month:     month,
		dow:       dow,
		minuteAny: minuteAny,
		hourAny:   hourAny,
		domAny:    domAny,
		dowAny:    dowAny,
	}, nil
}

// parseField parses one comma-separated cron field into the set of values in
// [min, max] it allows, and whether the field starts with "*" - which
// Expr.matches needs for cron's day-of-month/day-of-week rule. As in vixie
// cron, "*/2" counts as unrestricted there, so "0 9 */2 * 1" is odd days that
// are Mondays, not odd days or Mondays (review 2026-11-03, B-37).
func parseField(spec string, min, max int) (map[int]bool, bool, error) {
	set := map[int]bool{}
	isWildcard := strings.HasPrefix(spec, "*")

	for _, part := range strings.Split(spec, ",") {
		rangePart, step := part, 1

		if i := strings.IndexByte(part, '/'); i >= 0 {
			rangePart = part[:i]

			n, err := strconv.Atoi(part[i+1:])
			if err != nil || n <= 0 {
				return nil, false, fmt.Errorf("invalid step in %q", part)
			}

			step = n
		}

		var lo, hi int

		switch {
		case rangePart == "*":
			lo, hi = min, max
		case strings.Contains(rangePart, "-"):
			bounds := strings.SplitN(rangePart, "-", 2)

			a, errA := strconv.Atoi(bounds[0])
			b, errB := strconv.Atoi(bounds[1])
			if errA != nil || errB != nil {
				return nil, false, fmt.Errorf("invalid range %q", rangePart)
			}

			lo, hi = a, b
		default:
			v, err := strconv.Atoi(rangePart)
			if err != nil {
				return nil, false, fmt.Errorf("invalid value %q", rangePart)
			}

			lo, hi = v, v
			if step > 1 || strings.Contains(part, "/") {
				// "N/S" runs from N to the field's end, every S.
				hi = max
			}
		}

		if lo < min || hi > max || lo > hi {
			return nil, false, fmt.Errorf("%q out of range %d-%d", rangePart, min, max)
		}

		for v := lo; v <= hi; v += step {
			set[v] = true
		}
	}

	return set, isWildcard, nil
}

// maxSearch bounds how far into the future Next looks before giving up - a
// field combination that can never actually match (a day-of-month no month
// has) would otherwise loop forever. Four years comfortably covers "the 29th
// of February"; every ordinary schedule resolves within a day of searching,
// almost always within a minute.
const maxSearch = 4 * 366 * 24 * 60

// Next returns, in UTC, the first minute strictly after after that e
// matches on the UTC clock. ok is false only when nothing within maxSearch
// minutes matches.
func (e Expr) Next(after time.Time) (t time.Time, ok bool) {
	return e.NextIn(after, time.UTC)
}

// NextIn returns, in loc, the first minute strictly after after that e
// matches on loc's wall clock. ok is false only when nothing within
// maxSearch minutes matches.
//
// A daylight-saving change is read the way vixie cron reads it. A job at a
// fixed time (neither its minute nor its hour starts with "*") whose time
// falls in the hour skipped when clocks go forward fires at the first minute
// after the jump, so a daily 02:30 still runs that day; and when clocks go
// back it fires only at the first of the two moments that read the same,
// not twice. A wildcard job (every 15 minutes, every hour) runs by the
// clock as it is: no run for minutes that never happen, a run for each
// minute that happens twice.
func (e Expr) NextIn(after time.Time, loc *time.Location) (t time.Time, ok bool) {
	if loc == nil {
		loc = time.UTC
	}

	fixed := !e.minuteAny && !e.hourAny

	t = after.Truncate(time.Minute).Add(time.Minute)
	_, prevOffset := t.Add(-time.Minute).In(loc).Zone()

	for i := 0; i < maxSearch; i++ {
		local := t.In(loc)
		_, offset := local.Zone()

		switch {
		case e.matches(local):
			if !fixed || !repeatedWallTime(t, local, loc) {
				return local, true
			}
		case fixed && offset > prevOffset && e.matchesSkipped(local, offset-prevOffset):
			return local, true
		}

		prevOffset = offset
		t = t.Add(time.Minute)
	}

	return time.Time{}, false
}

// maxOffsetShift bounds how far back repeatedWallTime looks for clocks
// having gone back: every daylight-saving change in the tz database is two
// hours or less.
const maxOffsetShift = 3 * time.Hour

// repeatedWallTime reports whether the wall-clock minute local (the instant
// t in loc) already happened earlier, because clocks went back within the
// last few hours.
func repeatedWallTime(t, local time.Time, loc *time.Location) bool {
	_, offset := local.Zone()
	_, before := t.Add(-maxOffsetShift).In(loc).Zone()

	if before <= offset {
		return false
	}

	earlier := t.Add(-time.Duration(before-offset) * time.Second).In(loc)

	return sameWallMinute(earlier, local)
}

// matchesSkipped reports whether any wall-clock minute skipped just before
// local, because clocks went forward by jump seconds at local, matches e.
func (e Expr) matchesSkipped(local time.Time, jump int) bool {
	wall := time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), local.Minute(), 0, 0, time.UTC)

	for m := 1; m <= jump/60; m++ {
		if e.matches(wall.Add(-time.Duration(m) * time.Minute)) {
			return true
		}
	}

	return false
}

// sameWallMinute reports whether a and b read the same on their own clocks,
// to the minute.
func sameWallMinute(a, b time.Time) bool {
	return a.Year() == b.Year() && a.YearDay() == b.YearDay() && a.Hour() == b.Hour() && a.Minute() == b.Minute()
}

// matches reports whether t (already truncated to the minute) falls on this
// expression, read on t's own clock. Standard cron semantics: when both day-of-month and day-of-week
// are restricted (neither is "*"), a match needs only one of the two.
func (e Expr) matches(t time.Time) bool {
	if !e.minute[t.Minute()] || !e.hour[t.Hour()] || !e.month[int(t.Month())] {
		return false
	}

	domMatch := e.dom[t.Day()]
	dowMatch := e.dow[int(t.Weekday())]

	switch {
	case e.domAny && e.dowAny:
		return true
	case e.domAny:
		return dowMatch
	case e.dowAny:
		return domMatch
	default:
		return domMatch || dowMatch
	}
}
