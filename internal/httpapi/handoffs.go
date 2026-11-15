package httpapi

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/handoff"
	"github.com/YKoustubhRao/decurix/internal/mailtrigger"
	"github.com/YKoustubhRao/decurix/internal/skillhub"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// handoffAdapter backs the handoff service with this server's task submission, task records and desktops.
type handoffAdapter struct{ s *Server }

// SubmitHandoff sends a handed-on request to a worker, with its files when it has any.
func (a handoffAdapter) SubmitHandoff(ctx context.Context, id, content, dir string, files []handoff.File, origin tasks.Origin) (string, error) {
	if len(files) == 0 {
		return a.s.SubmitTracked(ctx, id, content, origin)
	}
	att := make([]mailtrigger.Attachment, len(files))
	for i, f := range files {
		att[i] = mailtrigger.Attachment{Name: f.Name, Data: f.Data}
	}
	return a.s.SubmitTrackedWithFiles(ctx, id, content, dir, att, origin)
}

// ReadFiles copies files out of a worker's desktop, within the handoff's carry budget.
func (a handoffAdapter) ReadFiles(ctx context.Context, id string, paths []string) ([]handoff.File, error) {
	var out []handoff.File
	budget := int64(handoff.MaxCarryBytes)
	for _, p := range paths {
		rc, err := a.s.Desktops.CopyOut(ctx, id, p)
		if err != nil {
			return nil, err
		}
		fs, err := handoff.ReadTarFiles(rc, budget)
		rc.Close()
		if err != nil {
			return nil, err
		}
		for _, f := range fs {
			budget -= int64(len(f.Data))
		}
		out = append(out, fs...)
	}
	return out, nil
}

// WriteFiles puts files into a worker's desktop.
func (a handoffAdapter) WriteFiles(ctx context.Context, id, dir string, files []handoff.File) error {
	out := make([]desktop.File, len(files))
	for i, f := range files {
		out[i] = desktop.File{Name: f.Name, Data: f.Data}
	}
	return a.s.Desktops.CopyFiles(ctx, id, dir, out)
}

// TaskOutcome is the receiving task's status, final report, final-check result and open questions.
func (a handoffAdapter) TaskOutcome(taskID string) (handoff.TaskOutcome, error) {
	d, err := a.s.Tasks.Get(taskID)
	if errors.Is(err, tasks.ErrNotFound) {
		return handoff.TaskOutcome{}, nil
	}
	if err != nil {
		return handoff.TaskOutcome{}, err
	}
	o := handoff.TaskOutcome{Found: true, Status: d.Status}
	if n := len(d.Executions); n > 0 {
		e := d.Executions[n-1]
		// Only a final the gate checked AND that is not a stop at a person's no counts as checked done: a report that
		// the work stopped because a person said no can be checked, but it is not the work done (handoff.md, Limits).
		// The structured verdict says which it is (R6); no report text is read for it.
		done := e.WorkDone()
		o.Report, o.Checked, o.Evidence = e.FinalReply, &done, verdictEvidence(e.Verdict)
		if e.Checked == nil && e.Verdict == nil {
			o.Checked = nil // an older image that said nothing: unknown, as before
		}
		for _, iv := range e.Interventions {
			if iv.RepliedAt.IsZero() {
				o.OpenQuestions = append(o.OpenQuestions, iv.Request)
			}
		}
		// The last steps it reported: a receiver that stopped part way hands these back marked interrupted.
		steps := e.Steps
		if len(steps) > handoffPartialSteps {
			steps = steps[len(steps)-handoffPartialSteps:]
		}
		for _, st := range steps {
			o.Partial = append(o.Partial, st.Text)
		}
	}
	return o, nil
}

// handoffPartialSteps is how many of a receiver's last steps an interrupted handoff hands back.
const handoffPartialSteps = 8

// verdictEvidence is what a final check says it saw: its evidence lines, then each passing check's detail, at most 40.
func verdictEvidence(v *tasks.GateVerdict) []string {
	if v == nil {
		return nil
	}
	out := []string{}
	for _, e := range v.Evidence {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	for _, c := range v.Checks {
		if d := strings.TrimSpace(c.Detail); c.Result == "pass" && d != "" {
			out = append(out, d)
		}
	}
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

// Busy says what a worker is busy with, "" when it is free.
func (a handoffAdapter) Busy(id string) (string, error) { return a.s.Tasks.Busy(id) }

// Resolve is a worker id a handoff may go to: a desktop that exists, never a practice or readiness one.
func (a handoffAdapter) Resolve(ctx context.Context, raw string) (string, bool) {
	id := desktop.SanitizeID(raw)
	if id == "" || strings.HasPrefix(id, "readiness-") || strings.HasPrefix(id, "practice-") {
		return "", false
	}
	if _, err := a.s.Desktops.Get(ctx, id); err != nil {
		return "", false
	}
	return id, true
}

// skillHubFinder finds the workers that hold a role or have a skill installed, from the org's skill registry.
type skillHubFinder struct{ hub *skillhub.Store }

// Holders lists the workers holding the role (or, with no role, having the skill installed), by worker id. Names
// are matched ignoring case, as the work queue and the approval policies match them.
func (f skillHubFinder) Holders(_ context.Context, role, skill string) ([]string, error) {
	var out []string
	if role != "" {
		all, err := f.hub.AllWorkerRoles()
		if err != nil {
			return nil, err
		}
		for w, rs := range all {
			if slices.ContainsFunc(rs, func(r string) bool { return strings.EqualFold(r, role) }) {
				out = append(out, w)
			}
		}
		sort.Strings(out)
		return out, nil
	}
	entries, err := f.hub.List()
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if strings.EqualFold(e.Name, skill) {
			for _, in := range e.Installs {
				out = append(out, in.Worker)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// NewHandoffService wires the handoff service to this server's plumbing.
func (s *Server) NewHandoffService(store *handoff.Store) *handoff.Service {
	a := handoffAdapter{s}
	svc := &handoff.Service{Store: store, Submit: a, Tasks: a, Workers: a, Reader: a, Writer: a}
	if s.SkillHub != nil {
		svc.Finder = skillHubFinder{s.SkillHub}
	}
	return svc
}
