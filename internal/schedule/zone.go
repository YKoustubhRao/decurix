// Time zones for cron triggers. A recurring schedule (and a DAG's cron) keeps
// the IANA zone its cron is read in, so "0 9 * * *" set by someone in
// Asia/Kolkata fires at 09:00 there, and a zone with daylight saving keeps
// firing at the same wall-clock time across a change (see Expr.NextIn). Rows
// written before zones existed keep "UTC", so nothing already scheduled moves.

package schedule

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// maxZoneName bounds a zone name a person may give; the longest IANA name is
// about 30 characters.
const maxZoneName = 64

// zones caches loaded locations by name: Due and List read a zone for every
// row, and loading one reads the tz database each time.
var zones sync.Map // name -> *time.Location

// DefaultZone returns the control plane's own IANA zone when it can tell
// (the TZ variable, or /etc/localtime linked into a zoneinfo tree), else
// "UTC". A Windows host has no IANA name for its zone, so it is "UTC" there;
// the pages send the browser's zone instead, so this is only the fallback.
func DefaultZone() string {
	if tz := strings.TrimPrefix(os.Getenv("TZ"), ":"); tz != "" {
		if name, _, err := LoadZone(tz); err == nil {
			return name
		}
	}

	if target, err := filepath.EvalSymlinks("/etc/localtime"); err == nil {
		target = filepath.ToSlash(target)
		if i := strings.LastIndex(target, "zoneinfo/"); i >= 0 {
			if name, _, err := LoadZone(target[i+len("zoneinfo/"):]); err == nil {
				return name
			}
		}
	}

	return "UTC"
}

// LoadZone resolves an IANA zone name a person gave ("" means DefaultZone)
// to its name and location. An unknown name, or Go's "Local" (which is not a
// zone anyone else can read), is an InputError.
func LoadZone(name string) (string, *time.Location, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = DefaultZone()
	}

	if cached, ok := zones.Load(name); ok {
		return name, cached.(*time.Location), nil
	}

	if name == "Local" || len(name) > maxZoneName {
		return "", nil, invalid("unknown time zone %q: give an IANA name such as Asia/Kolkata or UTC", clipZone(name))
	}

	loc, err := time.LoadLocation(name)
	if err != nil {
		return "", nil, invalid("unknown time zone %q: give an IANA name such as Asia/Kolkata or UTC", clipZone(name))
	}

	zones.Store(name, loc)

	return name, loc, nil
}

// ZoneLocation returns the location for a zone name already stored on a
// schedule. A stored name that no longer loads (a tz database without it)
// reads as UTC, logged, rather than stopping the schedule.
func ZoneLocation(name string) *time.Location {
	if name == "" || name == "UTC" {
		return time.UTC
	}

	_, loc, err := LoadZone(name)
	if err != nil {
		log.Printf("schedule: stored time zone %q does not load; reading it as UTC", name)
		return time.UTC
	}

	return loc
}

// clipZone bounds a zone name quoted back to the person.
func clipZone(name string) string {
	if r := []rune(name); len(r) > maxZoneName {
		return string(r[:maxZoneName]) + "..."
	}

	return name
}
