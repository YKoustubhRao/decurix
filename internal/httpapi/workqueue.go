package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/YKoustubhRao/decurix/internal/mailtrigger"
	"github.com/YKoustubhRao/decurix/internal/tasks"
	"github.com/YKoustubhRao/decurix/internal/workqueue"
)

// queueAdapter backs the work queue's dispatcher with this server's desktops, task records, skill registry and task submission.
type queueAdapter struct{ s *Server }

// Workers lists the desktops the queue may send to, with their roles and installed skills from the skill registry.
func (a queueAdapter) Workers(ctx context.Context) ([]workqueue.Worker, error) {
	list, err := a.s.Desktops.List(ctx)
	if err != nil {
		return nil, err
	}
	roles, skills := map[string][]string{}, map[string][]string{}
	if a.s.SkillHub != nil {
		if roles, err = a.s.SkillHub.AllWorkerRoles(); err != nil {
			return nil, err
		}
		entries, err := a.s.SkillHub.List()
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			for _, in := range e.Installs {
				skills[in.Worker] = append(skills[in.Worker], e.Name)
			}
		}
	}
	out := make([]workqueue.Worker, 0, len(list))
	for _, d := range list {
		if strings.HasPrefix(d.ID, "readiness-") || strings.HasPrefix(d.ID, "practice-") {
			continue
		}
		out = append(out, workqueue.Worker{ID: d.ID, Roles: roles[d.ID], Skills: skills[d.ID]})
	}
	return out, nil
}

// Busy says what a worker is busy with, "" when it is free.
func (a queueAdapter) Busy(id string) (string, error) { return a.s.Tasks.Busy(id) }

// Submit sends a queued request to a worker through the automated door, with its files when it has any.
func (a queueAdapter) Submit(ctx context.Context, worker, request, dir string, files []workqueue.File) (string, error) {
	if len(files) == 0 {
		return a.s.SubmitTracked(ctx, worker, request)
	}
	att := make([]mailtrigger.Attachment, len(files))
	for i, f := range files {
		att[i] = mailtrigger.Attachment{Name: f.Name, Data: f.Data}
	}
	return a.s.SubmitTrackedWithFiles(ctx, worker, request, dir, att)
}

// TaskState reads a queued item's task: latest execution status, whether its final was checked, and a person's verdict.
func (a queueAdapter) TaskState(taskID string) (workqueue.TaskState, error) {
	d, err := a.s.Tasks.Get(taskID)
	if errors.Is(err, tasks.ErrNotFound) {
		return workqueue.TaskState{}, nil
	}
	if err != nil {
		return workqueue.TaskState{}, err
	}
	st := workqueue.TaskState{Found: true, Status: d.Status, Verdict: d.Verdict}
	if n := len(d.Executions); n > 0 {
		e := d.Executions[n-1]
		st.Status, st.Reply, st.Checked = e.Status, e.FinalReply, e.Checked
		// A checked stop at a person's refusal is not the work done (same rule as handoffAdapter.TaskOutcome).
		if e.Checked != nil && *e.Checked && stoppedAtRefusal(e.FinalReply) {
			no := false
			st.Checked = &no
		}
	}
	return st, nil
}

// NewQueueDispatcher builds the work queue's dispatcher on this server's plumbing; spend is read from db's task records.
func (s *Server) NewQueueDispatcher(store *workqueue.Store, db *sql.DB) *workqueue.Dispatcher {
	a := queueAdapter{s}
	return &workqueue.Dispatcher{Store: store, Workers: a, Submitter: a, Tasks: a, Spend: workqueue.TaskSpend{DB: db}}
}
