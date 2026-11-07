// purge.go is the task records' part of purging a leaked secret by value (internal/purge): every column that keeps a
// person's or an agent's words is rewritten with the value replaced by a mark, in place, keeping every row and id so
// the records still join. It is the one change ever made to a judgement (judgements.go: append-only), because a
// secret is not a label; the purge itself is recorded as a judgement of kind JudgementPurge.

package tasks

import "github.com/YKoustubhRao/decurix/internal/sqlitedb"

// purgeColumns is every text column of the task records a value can be kept in, by table. The judgements are counted
// apart (PurgeValue).
var purgeColumns = []struct {
	table   string
	columns []string
}{
	{"tasks", []string{"request", "verdict_note"}},
	{"executions", []string{"final_reply", "verdict"}},
	{"execution_steps", []string{"text", "action"}},
	{"interventions", []string{"reason", "request", "reply"}},
}

// PurgeValue rewrites every task record and every judgement holding any of forms, each replaced by mark, and returns
// how many rows of each changed. Each table is one statement; SQLite does the scan. It holds the store's lock, so no
// read-decide-write of a route runs between them.
func (s *Store) PurgeValue(forms []string, mark string) (records, judgements int64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range purgeColumns {
		n, err := sqlitedb.ReplaceInColumns(s.db, t.table, t.columns, forms, mark)
		if err != nil {
			return records, judgements, err
		}
		records += n
	}
	judgements, err = sqlitedb.ReplaceInColumns(s.db, "judgements", []string{"text", "payload", "changed"}, forms, mark)
	return records, judgements, err
}
