package tasks

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// What each execution ran (plan R7): where it came from, the taught skill and version that drove it, and the model
// deployment that served it, so one query gives the pass rate per skill version and model.

// originKey carries the origin of what is sent with a context (WithOrigin).
type originKey struct{}

// WithOrigin returns ctx naming what started the request sent with it: a producer (a schedule, a DAG node, a mail
// rule, a queue item, a handoff) sets it before it submits, and the control plane writes it on the execution.
func WithOrigin(ctx context.Context, o Origin) context.Context {
	return context.WithValue(ctx, originKey{}, o)
}

// OriginFrom is the origin ctx names, and OriginUnknown when none.
func OriginFrom(ctx context.Context) Origin {
	if o, ok := ctx.Value(originKey{}).(Origin); ok && ValidOriginKind(o.Kind) {
		return o
	}
	return Origin{Kind: OriginUnknown}
}

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
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`UPDATE executions SET resolved_model = ? WHERE id = ?`, clipText(resolved, 160), execID); err != nil {
		return "", fmt.Errorf("tasks: recording the model: %w", err)
	}
	return resolved, nil
}

// PassRate is how one skill version did on one model, over the executions that ended: how many ran, how many ended
// checked done, how many a person marked correct or wrong, and how many were checked done yet marked wrong (a false
// done in production).
type PassRate struct {
	SkillName     string `json:"skill_name"`
	SkillVersion  int    `json:"skill_version"`
	Model         string `json:"model"`
	Runs          int    `json:"runs"`
	CheckedDone   int    `json:"checked_done"`
	PersonCorrect int    `json:"person_correct"`
	PersonWrong   int    `json:"person_wrong"`
	WrongButDone  int    `json:"wrong_but_checked"`
}

// PassRates is the pass rate per skill version and model, from one query over the ended executions: each task
// counted once, by its latest attempt, which is the one a person's verdict judges.
func (s *Store) PassRates() ([]PassRate, error) {
	return sqlitedb.QueryAll(s.db, func(r sqlitedb.Scanner) (PassRate, error) {
		var p PassRate
		err := r.Scan(&p.SkillName, &p.SkillVersion, &p.Model, &p.Runs, &p.CheckedDone, &p.PersonCorrect, &p.PersonWrong,
			&p.WrongButDone)
		return p, err
	}, `SELECT e.skill_name, e.skill_version, e.resolved_model, COUNT(*),
			SUM(CASE WHEN e.checked = 1 AND e.status = 'done' THEN 1 ELSE 0 END),
			SUM(CASE WHEN t.verdict = 'correct' THEN 1 ELSE 0 END),
			SUM(CASE WHEN t.verdict = 'wrong' THEN 1 ELSE 0 END),
			SUM(CASE WHEN t.verdict = 'wrong' AND e.checked = 1 AND e.status = 'done' THEN 1 ELSE 0 END)
		 FROM executions e JOIN tasks t ON t.id = e.task_id
		 WHERE e.status IN ('done', 'error') AND e.attempt = (SELECT MAX(x.attempt) FROM executions x WHERE x.task_id = e.task_id)
		 GROUP BY e.skill_name, e.skill_version, e.resolved_model
		 ORDER BY e.skill_name, e.skill_version, e.resolved_model`)
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
