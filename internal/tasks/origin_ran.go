// Whether a producer's request reached the task records: what a schedule asks to settle an occurrence a crash cut
// short (internal/schedule's RanChecker).
package tasks

import (
	"fmt"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// RanFromOrigin reports whether an execution started by origin kind and ref (a schedule and its id) was opened at or
// after since.
func (s *Store) RanFromOrigin(kind, ref string, since time.Time) (bool, error) {
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM executions WHERE origin = ? AND origin_ref = ? AND started_ns >= ?`,
		kind, ref, sqlitedb.ToNS(since),
	).Scan(&n); err != nil {
		return false, fmt.Errorf("tasks: ran from origin: %w", err)
	}
	return n > 0, nil
}
