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
// Metered, when set, is what the model proxy itself counted for the worker
// (llm.Hub.Metered): the tokens are the larger of the two, since a desktop's
// own report is empty until its turn ends and is the worker's word (C-08).
type TaskSpend struct {
	DB      *sql.DB
	Metered func(worker string, since time.Time) (int64, error)
}

// Spend sums what a worker's executions started at or after since have cost.
// An execution that has not reported its usage yet counts as nothing, but the
// proxy's own count of its calls does.
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
	if t.Metered != nil {
		metered, err := t.Metered(worker, since)
		if err != nil {
			return Spend{}, err
		}
		s.Tokens = max(s.Tokens, metered)
	}
	return s, nil
}
