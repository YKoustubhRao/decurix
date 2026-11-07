// purge.go is the concierge's part of purging a leaked secret by value (internal/purge): its messages and the
// requests of its sessions are rewritten with the value replaced by a mark, keeping every row.

package concierge

import "github.com/YKoustubhRao/decurix/internal/sqlitedb"

// PurgeValue rewrites every concierge message and session request holding any of forms, each replaced by mark, and
// returns how many rows changed. Two statements: SQLite does the scan.
func (s *Store) PurgeValue(forms []string, mark string) (int64, error) {
	msgs, err := sqlitedb.ReplaceInColumns(s.db, "concierge_messages", []string{"text"}, forms, mark)
	if err != nil {
		return 0, err
	}
	sessions, err := sqlitedb.ReplaceInColumns(s.db, "concierge_sessions", []string{"request"}, forms, mark)
	return msgs + sessions, err
}
