package workqueue

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// TaskSpend is the SpendSource backed by the task records (internal/tasks):
// it sums cost_usd and input+output tokens of a worker's executions started
// since a moment. It only reads the "executions" table, on the same database.
type TaskSpend struct{ DB *sql.DB }

// Spend sums what a worker's executions started at or after since have cost.
// An execution that has not reported its usage yet counts as nothing.
func (t TaskSpend) Spend(worker string, since time.Time) (Spend, error) {
	var s Spend
	err := t.DB.QueryRow(
		`SELECT COALESCE(SUM(cost_usd), 0), COALESCE(SUM(COALESCE(input_tokens, 0) + COALESCE(output_tokens, 0)), 0)
		 FROM executions WHERE worker_id = ? AND started_ns >= ?`,
		worker, sqlitedb.ToNS(since),
	).Scan(&s.USD, &s.Tokens)
	if err != nil {
		return Spend{}, fmt.Errorf("workqueue: reading spend: %w", err)
	}
	return s, nil
}
