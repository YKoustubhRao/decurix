package tasks

import (
	"fmt"
	"strings"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// maxRecentWorkers bounds how many workers one RecentForWorkers call reads: the dashboard asks for every card it
// shows, and the activity and task polls it replaces were bounded the same way.
const maxRecentWorkers = 200

// RecentForWorkers is RecentForWorker for many workers in one query: each named worker's tasks newest first, at most
// limit each (capped as RecentForWorker caps it), keyed by worker ID. A worker with no task has an empty list, and
// names past maxRecentWorkers are left out. It is what the dashboard's one poll reads, so a page of N cards costs one
// query, not N.
func (s *Store) RecentForWorkers(workerIDs []string, limit int) (map[string][]Task, error) {
	if limit <= 0 || limit > maxRecentTasks {
		limit = maxRecentTasks
	}

	out := map[string][]Task{}
	args := make([]any, 0, len(workerIDs)+1)
	for _, id := range workerIDs {
		if _, seen := out[id]; seen || id == "" {
			continue
		}
		if len(out) >= maxRecentWorkers {
			break
		}
		out[id] = []Task{}
		args = append(args, id)
	}
	if len(args) == 0 {
		return out, nil
	}

	// The window numbers each worker's tasks newest first; the outer query keeps the first limit of each. The
	// tasks_worker index (worker_id, created_ns) serves the inner read.
	marks := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
	list, err := sqlitedb.QueryAll(s.db, scanTask,
		`SELECT `+taskColumns+` FROM (
		   SELECT *, ROW_NUMBER() OVER (PARTITION BY worker_id ORDER BY created_ns DESC) AS pick
		   FROM tasks WHERE worker_id IN (`+marks+`)
		 ) t LEFT JOIN workers w ON w.id = t.worker_id
		 WHERE t.pick <= ? ORDER BY t.worker_id, t.created_ns DESC`, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("tasks: listing workers' tasks: %w", err)
	}

	for _, task := range list {
		out[task.WorkerID] = append(out[task.WorkerID], task)
	}
	return out, nil
}
