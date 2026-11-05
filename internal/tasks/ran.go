package tasks

import (
	"fmt"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// What each execution ran (plan R7): where it came from, the taught skill and version that drove it, and the model
// deployment that served it, so one query gives the pass rate per skill version and model.

// originKind is the kind an execution is stored with: unknown for none or one not on the list.
func originKind(o Origin) string {
	if ValidOriginKind(o.Kind) {
		return o.Kind
	}
	return OriginUnknown
}

// DeploymentLookup is the deployment that served a task's calls from worker since since ("" when none recorded):
// llmrec.Store.DeploymentFor with the operator's purpose.
type DeploymentLookup func(taskID, worker string, since time.Time) (string, error)

// ResolveModel records on an ended execution the model that served it: the deployment the call log names (lookup),
// or, with none recorded (a local brain the proxy never sees), "<engine>:<model as the desktop said>". It returns
// what it recorded.
func (s *Store) ResolveModel(execID string, lookup DeploymentLookup) (string, error) {
	var taskID, worker, engine, model string
	var started int64
	if err := s.db.QueryRow(`SELECT task_id, worker_id, engine, model, started_ns FROM executions WHERE id = ?`, execID).
		Scan(&taskID, &worker, &engine, &model, &started); err != nil {
		return "", fmt.Errorf("tasks: reading an execution: %w", err)
	}
	resolved := ""
	if lookup != nil {
		d, err := lookup(taskID, worker, sqlitedb.FromNS(started))
		if err != nil {
			return "", err
		}
		resolved = d
	}
	if resolved == "" {
		resolved = strings.TrimSpace(engine) + ":" + strings.TrimSpace(model)
	}
	// The read above is outside the store's lock on purpose: the fields read never change once the execution exists,
	// so only the write, which another outcome could race, is under it.
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`UPDATE executions SET resolved_model = ? WHERE id = ?`, clipText(resolved, 160), execID); err != nil {
		return "", fmt.Errorf("tasks: recording the model: %w", err)
	}
	return resolved, nil
}

// PassRate is how one skill version did on one model, in one class of origin, over its ended attempts (plan R7).
// The attempt is the unit: one trial of one skill version on one model, counted under its own version and model,
// however the task went on. Runs counts attempts that ended done, error or query (it stopped waiting on a person);
// Abandoned (the infrastructure gave out) is apart, never in Runs. FirstAttempt* is the headline pass@1: a task's
// first attempt is the independent trial, later ones are correlated with it. CheckedIndependent counts a checked done
// that rests on code, a ledger read-back or a person, the kinds W0.5 counts. A person's verdict judges the attempt
// it was given on (a task's latest), and only that attempt carries PersonCorrect, PersonWrong and WrongButChecked.
// Counts only: the reader puts Wilson bounds on them, as the eval report does.
type PassRate struct {
	SkillName           string `json:"skill_name"`
	SkillVersion        int    `json:"skill_version"`
	Model               string `json:"model"`
	OriginClass         string `json:"origin_class"` // production | practice | eval
	Runs                int    `json:"runs"`
	Abandoned           int    `json:"abandoned"`
	CheckedDone         int    `json:"checked_done"`
	CheckedIndependent  int    `json:"checked_independent"`
	FirstAttemptRuns    int    `json:"first_attempt_runs"`
	FirstAttemptChecked int    `json:"first_attempt_checked"`
	PersonCorrect       int    `json:"person_correct"`
	PersonWrong         int    `json:"person_wrong"`
	WrongButDone        int    `json:"wrong_but_checked"`
}

// Origin classes of a pass rate: a person's or an automation's work, Simulate or a readiness run, an eval case.
const (
	ClassProduction = "production"
	ClassPractice   = "practice"
	ClassEval       = "eval"
)

// PassRates is the pass rate per skill version, model and origin class, from one query over the ended attempts
// (PassRate says how each is counted).
func (s *Store) PassRates() ([]PassRate, error) {
	return sqlitedb.QueryAll(s.db, func(r sqlitedb.Scanner) (PassRate, error) {
		var p PassRate
		err := r.Scan(&p.SkillName, &p.SkillVersion, &p.Model, &p.OriginClass, &p.Runs, &p.Abandoned, &p.CheckedDone,
			&p.CheckedIndependent, &p.FirstAttemptRuns, &p.FirstAttemptChecked, &p.PersonCorrect, &p.PersonWrong,
			&p.WrongButDone)
		return p, err
	}, `WITH a AS (
			SELECT e.skill_name, e.skill_version, e.resolved_model, e.attempt, e.status,
				e.checked = 1 AND e.status = 'done' AS checked_done,
				CASE WHEN e.origin = 'eval' THEN 'eval'
					WHEN e.origin = 'practice' OR t.task_type = 'practice' THEN 'practice'
					ELSE 'production' END AS class,
				e.attempt = (SELECT MAX(x.attempt) FROM executions x WHERE x.task_id = e.task_id) AS latest,
				EXISTS (SELECT 1 FROM json_each(CASE WHEN json_valid(e.verdict) THEN e.verdict ELSE '{}' END, '$.basis')
					WHERE value IN ('code', 'ledger', 'person')) AS independent,
				t.verdict AS person
			FROM executions e JOIN tasks t ON t.id = e.task_id
			WHERE e.status IN ('done', 'error', 'query', 'abandoned'))
		SELECT skill_name, skill_version, resolved_model, class,
			SUM(status != 'abandoned'), SUM(status = 'abandoned'),
			SUM(checked_done), SUM(checked_done AND independent),
			SUM(attempt = 1 AND status != 'abandoned'), SUM(attempt = 1 AND checked_done),
			SUM(latest AND person = 'correct'), SUM(latest AND person = 'wrong'),
			SUM(latest AND person = 'wrong' AND checked_done)
		FROM a GROUP BY skill_name, skill_version, resolved_model, class
		ORDER BY class, skill_name, skill_version, resolved_model`)
}

// ExecutionKey names one execution: its id, its task and its worker.
type ExecutionKey struct{ ExecID, TaskID, WorkerID string }

// EndedExecutions is every execution that ended since since (of worker, or of every worker when it is ""), oldest
// first: what a run puller looks through for runs whose bundle it has not pulled yet.
func (s *Store) EndedExecutions(worker string, since time.Time) ([]ExecutionKey, error) {
	return sqlitedb.QueryAll(s.db, func(r sqlitedb.Scanner) (ExecutionKey, error) {
		var k ExecutionKey
		return k, r.Scan(&k.ExecID, &k.TaskID, &k.WorkerID)
	}, `SELECT id, task_id, worker_id FROM executions WHERE ended_ns >= ? AND ended_ns > 0 AND (? = '' OR worker_id = ?)
		ORDER BY ended_ns, id`, sqlitedb.ToNS(since), worker, worker)
}

// OpenExecutions is every execution not ended yet (queued, running or waiting on a person), oldest first.
func (s *Store) OpenExecutions() ([]ExecutionKey, error) {
	return sqlitedb.QueryAll(s.db, func(r sqlitedb.Scanner) (ExecutionKey, error) {
		var k ExecutionKey
		return k, r.Scan(&k.ExecID, &k.TaskID, &k.WorkerID)
	}, `SELECT id, task_id, worker_id FROM executions WHERE status IN (?, ?, ?) ORDER BY started_ns, id`,
		StatusQueued, StatusRunning, StatusWaiting)
}
