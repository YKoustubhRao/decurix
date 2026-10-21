package dag

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// recentRunsLimit bounds how many past runs Detail embeds for the editor
// page; Runs can page further back on its own.
const recentRunsLimit = 10

const runColumns = `id, dag_id, status, trigger_kind, started_ns, ended_ns`

func scanRun(scanner interface{ Scan(...any) error }) (Run, error) {
	var r Run
	var started, ended int64

	if err := scanner.Scan(&r.ID, &r.DAGID, &r.Status, &r.Trigger, &started, &ended); err != nil {
		return Run{}, err
	}

	r.StartedAt = sqlitedb.FromNS(started)
	r.EndedAt = sqlitedb.FromNS(ended)

	return r, nil
}

// lastRuns returns every DAG's single most recent run, keyed by dag_id, in
// one query - what List uses instead of one "last run" query per DAG. The
// inner subquery picks each dag_id's max started_ns; a tie (two runs
// started in the same nanosecond) is harmless here since either is an
// equally valid "most recent".
func (s *Store) lastRuns() (map[string]Run, error) {
	last, err := sqlitedb.QueryAll(s.db, scanRun,
		`SELECT `+runColumns+` FROM dag_runs WHERE (dag_id, started_ns) IN (
			SELECT dag_id, MAX(started_ns) FROM dag_runs GROUP BY dag_id
		)`,
	)
	if err != nil {
		return nil, err
	}

	runs := make(map[string]Run, len(last))

	for _, r := range last {
		runs[r.DAGID] = r
	}

	return runs, nil
}

// Runs returns a DAG's run history, most recent first, capped at limit (or
// recentRunsLimit if limit is non-positive).
func (s *Store) Runs(dagID string, limit int) ([]Run, error) {
	if limit <= 0 {
		limit = recentRunsLimit
	}

	runs, err := sqlitedb.QueryAll(s.db, scanRun,
		`SELECT `+runColumns+` FROM dag_runs WHERE dag_id = ? ORDER BY started_ns DESC LIMIT ?`, dagID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("dag: runs: %w", err)
	}

	return runs, nil
}

func (s *Store) getRun(runID string) (Run, error) {
	run, err := scanRun(s.db.QueryRow(`SELECT `+runColumns+` FROM dag_runs WHERE id = ?`, runID))
	if err == sql.ErrNoRows {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, fmt.Errorf("dag: get run %s: %w", runID, err)
	}

	return run, nil
}

// compareAndSwap runs a guarded update and reports whether it changed a row; what names it in an error.
func (s *Store) compareAndSwap(what, query string, args ...any) (bool, error) {
	result, err := s.db.Exec(query, args...)
	if err != nil {
		return false, fmt.Errorf("dag: %s: %w", what, err)
	}

	changed, err := result.RowsAffected()

	return changed > 0, err
}

// ActiveRuns returns every run, across every DAG, still in progress - what
// the Runner's tick sweeps forward.
func (s *Store) ActiveRuns() ([]Run, error) {
	runs, err := sqlitedb.QueryAll(s.db, scanRun, `SELECT `+runColumns+` FROM dag_runs WHERE status = ?`, RunRunning)
	if err != nil {
		return nil, fmt.Errorf("dag: active runs: %w", err)
	}

	return runs, nil
}

// RunNodes returns a run's per-node execution records.
func (s *Store) RunNodes(runID string) ([]RunNode, error) {
	nodes, err := sqlitedb.QueryAll(s.db, func(r sqlitedb.Scanner) (RunNode, error) {
		var rn RunNode
		var started, ended int64
		err := r.Scan(&rn.ID, &rn.RunID, &rn.NodeID, &rn.Status, &rn.TaskID, &rn.FinalReply, &started, &ended)
		rn.StartedAt = sqlitedb.FromNS(started)
		rn.EndedAt = sqlitedb.FromNS(ended)
		return rn, err
	}, `SELECT id, run_id, node_id, status, task_id, final_reply, started_ns, ended_ns
		 FROM dag_run_nodes WHERE run_id = ?`, runID)
	if err != nil {
		return nil, fmt.Errorf("dag: run nodes: %w", err)
	}

	return nodes, nil
}

// RunDetail returns one run's DAG snapshot (its current nodes/edges - a run
// against a node since edited or deleted shows the DAG as it is now, not as
// it was) and per-node status, for the run-detail graph view.
func (s *Store) RunDetail(runID string) (RunDetail, error) {
	run, err := s.getRun(runID)
	if err != nil {
		return RunDetail{}, err
	}

	nodes, err := s.Nodes(run.DAGID)
	if err != nil {
		return RunDetail{}, err
	}

	edges, err := s.Edges(run.DAGID)
	if err != nil {
		return RunDetail{}, err
	}

	runNodes, err := s.RunNodes(runID)
	if err != nil {
		return RunDetail{}, err
	}

	return RunDetail{Run: run, Nodes: nodes, Edges: edges, RunNodes: runNodes}, nil
}

// StartRun begins a new run of dagID: it materializes one pending RunNode
// for every node reachable from the head by following edges forward, which
// is what makes "every reachable node is terminal" well-defined later, and
// what lets the runner's readiness check ignore a parent edge from a node
// outside that set instead of waiting on it forever.
func (s *Store) StartRun(dagID, trigger string, now time.Time) (Run, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Run{}, fmt.Errorf("dag: start run: %w", err)
	}
	defer tx.Rollback()

	var headNodeID string

	if err := tx.QueryRow(`SELECT head_node_id FROM dags WHERE id = ?`, dagID).Scan(&headNodeID); err != nil {
		if err == sql.ErrNoRows {
			return Run{}, ErrNotFound
		}
		return Run{}, fmt.Errorf("dag: start run: %w", err)
	}

	if headNodeID == "" {
		return Run{}, fmt.Errorf("dag: this DAG has no head node set")
	}

	edges, err := edgeEnds(tx, dagID)
	if err != nil {
		return Run{}, fmt.Errorf("dag: start run: %w", err)
	}

	children := make(map[string][]string)

	for _, e := range edges {
		children[e.FromNodeID] = append(children[e.FromNodeID], e.ToNodeID)
	}

	reachable := map[string]bool{headNodeID: true}
	queue := []string{headNodeID}

	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]

		for _, next := range children[n] {
			if !reachable[next] {
				reachable[next] = true
				queue = append(queue, next)
			}
		}
	}

	run := Run{ID: sqlitedb.NewID("drun-", 8), DAGID: dagID, Status: RunRunning, Trigger: trigger, StartedAt: now}

	if _, err := tx.Exec(
		`INSERT INTO dag_runs (id, dag_id, status, trigger_kind, started_ns) VALUES (?, ?, ?, ?, ?)`,
		run.ID, run.DAGID, run.Status, run.Trigger, sqlitedb.ToNS(now),
	); err != nil {
		return Run{}, fmt.Errorf("dag: start run: %w", err)
	}

	for nodeID := range reachable {
		if _, err := tx.Exec(
			`INSERT INTO dag_run_nodes (id, run_id, node_id, status) VALUES (?, ?, ?, ?)`,
			sqlitedb.NewID("drn-", 8), run.ID, nodeID, NodePending,
		); err != nil {
			return Run{}, fmt.Errorf("dag: start run: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return Run{}, fmt.Errorf("dag: start run: %w", err)
	}

	return run, nil
}

// MarkRunNodeDispatched transitions a run-node from pending to running, once
// its ticket has actually been submitted. A compare-and-swap (it only
// applies from status pending): see runner.go's package doc for why every
// run-node transition needs to be one.
func (s *Store) MarkRunNodeDispatched(id, taskID string, now time.Time) (bool, error) {
	return s.compareAndSwap("mark run node dispatched "+id,
		`UPDATE dag_run_nodes SET status = ?, task_id = ?, started_ns = ? WHERE id = ? AND status = ?`,
		NodeRunning, taskID, sqlitedb.ToNS(now), id, NodePending,
	)
}

// ClaimRunNode compare-and-swaps a run-node from pending to running with no
// task ID yet - a node's slot in the run must be claimed *before* its ticket
// is actually submitted (see runner.go's dispatch), so two racing advance()
// calls (a manual Trigger landing on the same tick as the background
// Runner.Run, see the package doc) can't both submit the same node's ticket;
// the loser of this CAS never calls Submitter at all. A run-node claimed but
// not yet holding a task ID (status running, task_id "") is deliberately
// invisible to resolveRunning (it checks task_id != ""), so it is never
// mistaken for one whose ticket is already in flight.
func (s *Store) ClaimRunNode(id string, now time.Time) (bool, error) {
	return s.compareAndSwap("claim run node "+id,
		`UPDATE dag_run_nodes SET status = ?, started_ns = ? WHERE id = ? AND status = ?`,
		NodeRunning, sqlitedb.ToNS(now), id, NodePending,
	)
}

// SetRunNodeTaskID records the task ID a claimed run-node's ticket landed
// under. Not a compare-and-swap against a race with another dispatcher -
// only the goroutine that won ClaimRunNode ever calls this for a given
// run-node - but still scoped to a claimed-and-not-yet-submitted row (status
// running, task_id empty) as a defensive guard against calling it twice.
func (s *Store) SetRunNodeTaskID(id, taskID string) error {
	_, err := s.db.Exec(
		`UPDATE dag_run_nodes SET task_id = ? WHERE id = ? AND status = ? AND task_id = ''`,
		taskID, id, NodeRunning,
	)
	if err != nil {
		return fmt.Errorf("dag: set run node task id %s: %w", id, err)
	}

	return nil
}

// ReleaseRunNode reverts a claimed-but-not-yet-submitted run-node back to
// pending, so it is retried on a later tick instead of dispatching into a
// desktop that turned out to be busy - see runner.go's dispatch, which backs
// off rather than letting internal/tasks queue (and possibly silently drop,
// see absorbQueuedReply) a second ticket behind whatever is already running
// there. Scoped to a claimed row with no task ID yet, the same guard
// SetRunNodeTaskID uses, so it can never revert a node whose ticket was
// actually submitted.
func (s *Store) ReleaseRunNode(id string) (bool, error) {
	return s.compareAndSwap("release run node "+id,
		`UPDATE dag_run_nodes SET status = ?, started_ns = 0, ended_ns = 0 WHERE id = ? AND status = ? AND task_id = ''`,
		NodePending, id, NodeRunning,
	)
}

// MarkRunNodeTerminal transitions a run-node from fromStatus to a terminal
// toStatus (succeeded or failed), recording the reply it ended with.
// Compare-and-swap, same reason as MarkRunNodeDispatched.
func (s *Store) MarkRunNodeTerminal(id, fromStatus, toStatus, finalReply string, now time.Time) (bool, error) {
	return s.compareAndSwap("mark run node terminal "+id,
		`UPDATE dag_run_nodes SET status = ?, final_reply = ?, ended_ns = ? WHERE id = ? AND status = ?`,
		toStatus, finalReply, sqlitedb.ToNS(now), id, fromStatus,
	)
}

// SkipRunNode transitions a still-pending run-node to skipped - a node
// reachable only through one that just failed. Compare-and-swap, same
// reason as MarkRunNodeDispatched.
func (s *Store) SkipRunNode(id string, now time.Time) (bool, error) {
	return s.compareAndSwap("skip run node "+id,
		`UPDATE dag_run_nodes SET status = ?, ended_ns = ? WHERE id = ? AND status = ?`,
		NodeSkipped, sqlitedb.ToNS(now), id, NodePending,
	)
}

// FinishRun closes a run still running as succeeded or failed.
// Compare-and-swap, same reason as MarkRunNodeDispatched.
func (s *Store) FinishRun(id, status string, now time.Time) (bool, error) {
	return s.compareAndSwap("finish run "+id,
		`UPDATE dag_runs SET status = ?, ended_ns = ? WHERE id = ? AND status = ?`,
		status, sqlitedb.ToNS(now), id, RunRunning,
	)
}
