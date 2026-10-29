package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/practice"
	"github.com/YKoustubhRao/decurix/internal/qualification"
)

// Simulate's owner routes (docs/simulation.md). They sit under
// /api/readiness/ because the owner session cookie's path is that prefix, need
// the owner session and CSRF token (qualificationOwner), and are registered on
// the primary listener only: RegisterSimulationRoutes is never called for the
// gated mux.

var (
	simSkillName   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)
	simRecordingID = regexp.MustCompile(`^(task|adhoc|demo)-[A-Za-z0-9-]{1,64}$`)
)

// maxPracticeSourceBody bounds a practice-source answer (4 MiB of files, base64).
const maxPracticeSourceBody = 8 << 20

type simulationRoutes struct {
	s *Server
	p *practice.Service
}

// RegisterSimulationRoutes registers Simulate's owner routes on mux, which
// must be the primary listener's; every route needs the readiness owner session.
func (s *Server) RegisterSimulationRoutes(mux *http.ServeMux, p *practice.Service) {
	h := &simulationRoutes{s: s, p: p}
	for path, fn := range map[string]http.HandlerFunc{
		"/api/readiness/simulation/sources":   h.sources,
		"/api/readiness/simulation/draft":     h.draft,
		"/api/readiness/simulation/approve":   h.approve,
		"/api/readiness/simulation/retire":    h.retire,
		"/api/readiness/simulation/list":      h.list,
		"/api/readiness/simulation/suites":    h.suites,
		"/api/readiness/simulation/run":       h.run,
		"/api/readiness/simulation/cancel":    h.cancel,
		"/api/readiness/simulation/history":   h.history,
		"/api/readiness/simulation/evidence":  h.evidence,
		"/api/readiness/simulation/file":      h.file,
		"/api/readiness/simulation/authorize": h.authorize,
	} {
		mux.HandleFunc(path, s.qualificationOwner(fn))
	}
}

// ready answers 503 when the practice service is missing.
func (h *simulationRoutes) ready(w http.ResponseWriter) bool {
	if h.p == nil || h.p.Store == nil {
		jsonStatusError(w, 503, "Simulate is not set up on this control plane.")
		return false
	}
	return true
}

// agentGet reads one JSON document from a worker's agent, bounded.
func (h *simulationRoutes) agentGet(ctx context.Context, worker, path string, v any) error {
	var resp *http.Response
	var err error
	switch {
	case h.s.agentRequest != nil:
		resp, err = h.s.agentRequest(ctx, worker, http.MethodGet, path, nil)
	case h.s.Desktops != nil:
		resp, err = h.s.Desktops.AgentRequest(ctx, worker, http.MethodGet, path, nil)
	default:
		return qualification.ErrDenied
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return qualification.ErrDenied
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxPracticeSourceBody)).Decode(v)
}

// sourcePath is the agent's practice-source path for a source, or "" when its name is not valid.
func sourcePath(kind, name string) string {
	switch {
	case kind == qualification.SourceSkill && simSkillName.MatchString(name):
		return "/skills/" + name + "/practice-source"
	case kind == qualification.SourceRecording && simRecordingID.MatchString(name):
		return "/recordings/" + name + "/practice-source"
	}
	return ""
}

// fetchSource reads one source's exact bytes from the worker's agent.
func (h *simulationRoutes) fetchSource(ctx context.Context, worker, kind, name string) (qualification.PracticeSource, error) {
	path := sourcePath(kind, name)
	if path == "" {
		return qualification.PracticeSource{}, qualification.ErrInvalid
	}
	var a practice.AgentSource
	if err := h.agentGet(ctx, worker, path, &a); err != nil {
		return qualification.PracticeSource{}, err
	}
	if a.Kind != kind || a.Name != name {
		return qualification.PracticeSource{}, qualification.ErrDenied
	}
	return a.PracticeSource(), nil
}

// practiceError answers a practice failure with a sentence, never err.Error()
// of an internal failure.
func practiceError(w http.ResponseWriter, err error) {
	var low practice.LowMemoryError
	switch {
	case errors.As(err, &low):
		jsonStatusError(w, 409, low.Error())
	case errors.Is(err, practice.ErrBusy), errors.Is(err, practice.ErrStale):
		jsonStatusError(w, 409, practice.Public(err))
	case errors.Is(err, practice.ErrHostedOnly):
		jsonStatusError(w, 422, practice.Public(err))
	case errors.Is(err, qualification.ErrInvalid), errors.Is(err, qualification.ErrNotFound),
		errors.Is(err, qualification.ErrDenied), errors.Is(err, qualification.ErrConflict):
		qualificationError(w, err)
	default:
		var sentence practice.Sentence
		if errors.As(err, &sentence) {
			jsonStatusError(w, 409, string(sentence))
			return
		}
		jsonError(w, err)
	}
}

// sources lists a worker's skills and recordings with their kind of work, lint
// grade, readiness state and, when they cannot be practised, why.
func (h *simulationRoutes) sources(w http.ResponseWriter, r *http.Request) {
	if !requireMethodJSON(w, r, http.MethodGet) || !h.ready(w) {
		return
	}
	worker := r.URL.Query().Get("worker")
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var listed struct {
		Sources []practice.AgentSource `json:"sources"`
	}
	if err := h.agentGet(ctx, worker, "/practice-sources", &listed); err != nil {
		jsonStatusError(w, 422, "Start the worker, with an agent that lists practice sources, to choose what to practise.")
		return
	}
	states, err := h.p.Store.ListPractice(worker)
	if err != nil {
		qualificationError(w, err)
		return
	}
	type row struct {
		practice.SourceRow
		Responsibility *qualification.PracticeResponsibility `json:"responsibility,omitempty"`
	}
	out := []row{}
	for _, a := range listed.Sources {
		if sourcePath(a.Kind, a.Name) == "" {
			continue
		}
		rw := row{SourceRow: a.Row()}
		for i := range states {
			if states[i].SourceKind == a.Kind && states[i].SourceName == a.Name {
				rw.Responsibility = &states[i]
			}
		}
		out = append(out, rw)
	}
	jsonResponse(w, map[string]any{"sources": out})
}

// draft starts authoring (POST) or shows a job or a stored suite (GET ?job= / ?id=).
func (h *simulationRoutes) draft(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	if r.Method == http.MethodGet {
		if id := r.URL.Query().Get("job"); id != "" {
			job, ok := h.p.Job(id)
			if !ok {
				jsonStatusError(w, 404, "No such draft in progress.")
				return
			}
			jsonResponse(w, job)
			return
		}
		su, err := h.p.Store.Suite(r.URL.Query().Get("id"))
		if err != nil {
			qualificationError(w, err)
			return
		}
		jsonResponse(w, suiteView(su))
		return
	}
	if !requireMethodJSON(w, r, http.MethodPost) {
		return
	}
	var body struct {
		Worker  string `json:"worker"`
		Sources []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"sources"`
		Count int `json:"count"`
	}
	if !qualificationJSON(w, r, &body) {
		return
	}
	if body.Worker == "" || len(body.Sources) == 0 || len(body.Sources) > 10 || body.Count < 0 || body.Count > qualification.MaxPracticeCases {
		qualificationError(w, qualification.ErrInvalid)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	jobs := []practice.DraftJob{}
	for _, want := range body.Sources {
		src, err := h.fetchSource(ctx, body.Worker, want.Kind, want.Name)
		if err != nil {
			jsonStatusError(w, 422, "Start the worker and choose one of its skills or recordings.")
			return
		}
		if src.OutOfScope != "" {
			jsonStatusError(w, 422, "This source cannot be practised yet: "+src.OutOfScope+".")
			return
		}
		job, err := h.p.StartDraft(practice.DraftRequest{WorkerID: body.Worker, Source: src, Count: body.Count})
		if err != nil {
			practiceError(w, err)
			return
		}
		jobs = append(jobs, job)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"jobs": jobs})
}

// practiceFileView is an input file as the review table shows it: name and hash, no bytes.
type practiceFileView struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int    `json:"size"`
}

// suiteView is a suite for review: generator and mock source, the cases table
// with file names and hashes, problems and the hashes approval must echo.
func suiteView(su qualification.PracticeSuite) map[string]any {
	type caseView struct {
		ID       string                        `json:"id"`
		Category string                        `json:"category"`
		Request  string                        `json:"request"`
		Files    []practiceFileView            `json:"files"`
		Fixture  json.RawMessage               `json:"fixture,omitempty"`
		Expected string                        `json:"expected"`
		Checks   []qualification.PracticeCheck `json:"checks"`
	}
	cases := make([]caseView, 0, len(su.Cases))
	for _, c := range su.Cases {
		v := caseView{ID: c.ID, Category: c.Category, Request: c.Request, Fixture: c.Fixture, Expected: c.Expected, Checks: c.Checks, Files: []practiceFileView{}}
		names := make([]string, 0, len(c.Files))
		for n := range c.Files {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			v.Files = append(v.Files, practiceFileView{Name: n, SHA256: qualification.SHA256Hex(c.Files[n]), Size: len(c.Files[n])})
		}
		cases = append(cases, v)
	}
	sources := []practiceFileView{}
	for n, b := range su.Source.Files {
		sources = append(sources, practiceFileView{Name: n, SHA256: qualification.SHA256Hex(b), Size: len(b)})
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].Name < sources[j].Name })
	return map[string]any{
		"id": su.ID, "responsibility_id": su.ResponsibilityID, "worker_id": su.WorkerID, "status": su.Status, "revision": su.Revision,
		"source": map[string]any{"kind": su.Source.Kind, "name": su.Source.Name, "title": su.Source.Title, "work_kind": su.Source.WorkKind,
			"hosts": su.Source.Hosts, "consequential": su.Source.Consequential, "sha256": su.Source.SHA256, "files": sources},
		"pins": su.Pins, "generator": su.Generator, "generator_sha": su.GeneratorSHA, "mock": su.Mock, "mock_sha": su.MockSHA,
		"seed": su.Seed, "count": su.Count, "cases": cases, "cases_sha": su.CasesSHA, "problems": su.Problems, "reason": su.Reason,
		"created_at": su.CreatedAt, "approved_at": su.ApprovedAt,
	}
}

// approve pins a reviewed draft; the hashes must be the ones the owner saw.
func (h *simulationRoutes) approve(w http.ResponseWriter, r *http.Request) {
	if !requireMethodJSON(w, r, http.MethodPost) || !h.ready(w) {
		return
	}
	var body struct {
		DraftID      string `json:"draft_id"`
		GeneratorSHA string `json:"generator_sha"`
		MockSHA      string `json:"mock_sha"`
		CasesSHA     string `json:"cases_sha"`
	}
	if !qualificationJSON(w, r, &body) {
		return
	}
	resp, err := h.p.Store.ApproveSuite(body.DraftID, body.GeneratorSHA, body.MockSHA, body.CasesSHA)
	if errors.Is(err, qualification.ErrDenied) {
		jsonStatusError(w, 409, approveRefusal(h.p.Store, body.DraftID, body.GeneratorSHA, body.MockSHA, body.CasesSHA))
		return
	}
	if err != nil {
		qualificationError(w, err)
		return
	}
	jsonResponse(w, resp)
}

// approveRefusal says why a draft cannot be approved, in the owner's terms.
func approveRefusal(store *qualification.Store, id, generatorSHA, mockSHA, casesSHA string) string {
	su, err := store.Suite(id)
	switch {
	case err != nil:
		return "No such draft."
	case su.Status != qualification.SuiteDraft:
		return "This suite is no longer a draft; draft a new one to change it."
	case len(su.Problems) > 0:
		return "This draft has problems and cannot be approved: " + su.Problems[0] + " Draft it again."
	case su.GeneratorSHA != generatorSHA || su.MockSHA != mockSHA || su.CasesSHA != casesSHA:
		return "The draft changed since it was shown; review it again before approving."
	}
	return "This draft cannot be approved."
}

// retire withdraws a suite an owner found wrong; its findings stay as they were.
func (h *simulationRoutes) retire(w http.ResponseWriter, r *http.Request) {
	if !requireMethodJSON(w, r, http.MethodPost) || !h.ready(w) {
		return
	}
	var body struct {
		SuiteID string `json:"suite_id"`
		Reason  string `json:"reason"`
	}
	if !qualificationJSON(w, r, &body) {
		return
	}
	reason := strings.TrimSpace(body.Reason)
	if reason == "" || len(reason) > 2000 {
		qualificationError(w, qualification.ErrInvalid)
		return
	}
	if err := h.p.Store.RetireSuite(body.SuiteID, reason); err != nil {
		qualificationError(w, err)
		return
	}
	jsonResponse(w, map[string]string{"status": "retired"})
}

// list is a worker's practice responsibilities and states, for the card's badge.
func (h *simulationRoutes) list(w http.ResponseWriter, r *http.Request) {
	if !requireMethodJSON(w, r, http.MethodGet) || !h.ready(w) {
		return
	}
	list, err := h.p.Store.ListPractice(r.URL.Query().Get("worker"))
	if err != nil {
		qualificationError(w, err)
		return
	}
	jsonResponse(w, map[string]any{"responsibilities": list, "running": h.p.Running()})
}

// suites lists a responsibility's suites without their bytes.
func (h *simulationRoutes) suites(w http.ResponseWriter, r *http.Request) {
	if !requireMethodJSON(w, r, http.MethodGet) || !h.ready(w) {
		return
	}
	list, err := h.p.Store.Suites(r.URL.Query().Get("responsibility"))
	if err != nil {
		qualificationError(w, err)
		return
	}
	jsonResponse(w, list)
}

// run starts a practice run of an approved suite (POST) or shows its progress (GET ?id=).
func (h *simulationRoutes) run(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	if r.Method == http.MethodGet {
		run, err := h.p.Store.PracticeRunGet(r.URL.Query().Get("id"))
		if err != nil {
			qualificationError(w, err)
			return
		}
		jsonResponse(w, run)
		return
	}
	if !requireMethodJSON(w, r, http.MethodPost) {
		return
	}
	var body struct {
		SuiteID string `json:"suite_id"`
	}
	if !qualificationJSON(w, r, &body) {
		return
	}
	su, err := h.p.Store.Suite(body.SuiteID)
	if err != nil {
		qualificationError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	src, err := h.fetchSource(ctx, su.WorkerID, su.Source.Kind, su.Source.Name)
	if err != nil {
		jsonStatusError(w, 409, "Could not confirm the practised skill or recording. Start the worker and try again.")
		return
	}
	run, err := h.p.StartRun(ctx, su.ID, src.SHA256)
	if err != nil {
		practiceError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(run)
}

// cancel ends a run durably before its work stops.
func (h *simulationRoutes) cancel(w http.ResponseWriter, r *http.Request) {
	if !requireMethodJSON(w, r, http.MethodPost) || !h.ready(w) {
		return
	}
	var body struct {
		RunID string `json:"run_id"`
	}
	if !qualificationJSON(w, r, &body) {
		return
	}
	run, err := h.p.CancelRun(body.RunID)
	if err != nil {
		qualificationError(w, err)
		return
	}
	jsonResponse(w, run)
}

// history lists a responsibility's runs, newest first.
func (h *simulationRoutes) history(w http.ResponseWriter, r *http.Request) {
	if !requireMethodJSON(w, r, http.MethodGet) || !h.ready(w) {
		return
	}
	runs, err := h.p.Store.PracticeHistory(r.URL.Query().Get("responsibility"))
	if err != nil {
		qualificationError(w, err)
		return
	}
	jsonResponse(w, runs)
}

// evidence is a run with every finding and its files' names and hashes.
func (h *simulationRoutes) evidence(w http.ResponseWriter, r *http.Request) {
	if !requireMethodJSON(w, r, http.MethodGet) || !h.ready(w) {
		return
	}
	id := r.URL.Query().Get("run")
	run, err := h.p.Store.PracticeRunGet(id)
	if err != nil {
		qualificationError(w, err)
		return
	}
	files, err := h.p.Store.PracticeFiles(id)
	if err != nil {
		qualificationError(w, err)
		return
	}
	jsonResponse(w, map[string]any{"run": run, "files": files})
}

// file downloads one evidence file, never rendered inline.
func (h *simulationRoutes) file(w http.ResponseWriter, r *http.Request) {
	if !requireMethodJSON(w, r, http.MethodGet) || !h.ready(w) {
		return
	}
	q := r.URL.Query()
	name := q.Get("name")
	if !qualification.FileName(name) {
		qualificationError(w, qualification.ErrInvalid)
		return
	}
	f, err := h.p.Store.PracticeFileBody(q.Get("run"), q.Get("case"), q.Get("role"), name)
	if err != nil {
		qualificationError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+strings.ReplaceAll(name, "\"", "")+"\"")
	_, _ = w.Write(f.Body)
}

// authorize enables the current revision after a clean run (rechecking that the
// worker's skill and pins are unchanged), or suspends it. Enabling lets the
// skill start unattended once that flag exists; it never relaxes the one-action
// approval for sending, paying or publishing.
func (h *simulationRoutes) authorize(w http.ResponseWriter, r *http.Request) {
	if !requireMethodJSON(w, r, http.MethodPost) || !h.ready(w) {
		return
	}
	var body struct {
		Responsibility string `json:"responsibility"`
		Revision       string `json:"revision"`
		Action         string `json:"action"`
	}
	if !qualificationJSON(w, r, &body) {
		return
	}
	if body.Action == "enable" {
		resp, err := h.p.Store.GetPractice(body.Responsibility)
		if err != nil {
			qualificationError(w, err)
			return
		}
		su, err := h.p.Store.Suite(resp.SuiteID)
		if err != nil {
			qualificationError(w, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		src, err := h.fetchSource(ctx, su.WorkerID, su.Source.Kind, su.Source.Name)
		if err == nil {
			err = h.p.CheckCurrent(ctx, su, src.SHA256)
		}
		if err != nil {
			jsonStatusError(w, 409, "Could not confirm the practised skill, image and agent are unchanged. Start the worker, or practise again.")
			return
		}
	}
	resp, err := h.p.Store.AuthorizePractice(body.Responsibility, body.Revision, body.Action)
	if err != nil {
		qualificationError(w, err)
		return
	}
	jsonResponse(w, resp)
}
