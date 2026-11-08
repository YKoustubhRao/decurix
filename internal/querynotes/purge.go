// purge.go is the query notes' part of purging a leaked secret by value (internal/purge): a note's words, its gate's
// request and question, and its resolution are rewritten with the value replaced by a mark, keeping the note.

package querynotes

import "github.com/YKoustubhRao/decurix/internal/sqlitedb"

// PurgeValue rewrites every query note holding any of forms, each replaced by mark, and returns how many changed.
// One statement: SQLite does the scan.
func (s *Store) PurgeValue(forms []string, mark string) (int64, error) {
	return sqlitedb.ReplaceInColumns(s.db, "query_notes",
		[]string{"content", "gate_request", "gate_question", "resolution", "filename"}, forms, mark)
}
