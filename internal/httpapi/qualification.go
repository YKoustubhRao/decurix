package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/YKoustubhRao/decurix/internal/qualification"
	"github.com/YKoustubhRao/decurix/internal/tasks"
	"github.com/YKoustubhRao/decurix/internal/webui"
)

// QualificationOwnerFile is a local, per-launch key; no HTTP route reveals it.
const QualificationOwnerFile = qualification.OwnerKeyFile

type qualificationSession struct {
	CSRF  string
	Until time.Time
}
type qualificationAuth struct {
	mu       sync.Mutex
	key      string
	sessions map[string]qualificationSession
}

type qualificationRuns struct {
	mu     sync.Mutex
	active map[string]context.CancelFunc
}

// SetQualificationOwner creates a fresh owner key for this control-plane launch.
// Unlike container-IP heuristics, this guard also works on Docker Desktop.
func (s *Server) SetQualificationOwner(dir string) error {
	key := qualificationToken()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	file := filepath.Join(dir, QualificationOwnerFile)
	if info, err := os.Lstat(file); err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return errors.New("readiness owner key path must be a regular local file")
	}
	if err := os.WriteFile(file, []byte(key+"\n"), 0600); err != nil {
		return err
	}
	if err := os.Chmod(file, 0600); err != nil {
		return err
	}
	s.qualificationAuth.key = key
	s.qualificationAuth.sessions = map[string]qualificationSession{}
	return nil
}

func qualificationToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// qualificationSessionHandler exchanges the local key for an HttpOnly session.
// Session and CSRF values are never passed through tasks or the agent's filesystem.
func (s *Server) qualificationSessionHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		jsonStatusError(w, 405, "POST required")
		return
	}
	if !qualificationSameOrigin(r) {
		jsonStatusError(w, 403, "Open readiness on this control plane's own page.")
		return
	}
	var body struct {
		Key string `json:"key"`
	}
	if !qualificationJSON(w, r, &body) {
		return
	}
	s.qualificationAuth.mu.Lock()
	defer s.qualificationAuth.mu.Unlock()
	if len(body.Key) != 64 || s.qualificationAuth.key == "" || subtle.ConstantTimeCompare([]byte(body.Key), []byte(s.qualificationAuth.key)) != 1 {
		jsonStatusError(w, 403, "The local owner key was not accepted.")
		return
	}
	if s.qualificationAuth.sessions == nil {
		s.qualificationAuth.sessions = map[string]qualificationSession{}
	}
	for id, session := range s.qualificationAuth.sessions {
		if time.Now().After(session.Until) {
			delete(s.qualificationAuth.sessions, id)
		}
	}
	id, csrf := qualificationToken(), qualificationToken()
	s.qualificationAuth.sessions[id] = qualificationSession{CSRF: csrf, Until: time.Now().Add(8 * time.Hour)}
	http.SetCookie(w, &http.Cookie{Name: "decurix_readiness_owner", Value: id, Path: "/api/readiness/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: 8 * 60 * 60})
	jsonResponse(w, map[string]string{"csrf": csrf})
}

func qualificationSameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return origin == scheme+"://"+r.Host
}

func (s *Server) qualificationOwner(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if s.Qualification == nil {
			jsonStatusError(w, 503, "Readiness is not configured.")
			return
		}
		cookie, err := r.Cookie("decurix_readiness_owner")
		if err != nil {
			jsonStatusError(w, 401, "Unlock readiness with the local owner key.")
			return
		}
		s.qualificationAuth.mu.Lock()
		session, ok := s.qualificationAuth.sessions[cookie.Value]
		s.qualificationAuth.mu.Unlock()
		if !ok || time.Now().After(session.Until) || !qualificationSameOrigin(r) {
			jsonStatusError(w, 403, "The owner session expired or belongs to another origin.")
			return
		}
		if r.Method != http.MethodGet && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Readiness-CSRF")), []byte(session.CSRF)) != 1 {
			jsonStatusError(w, 403, "The owner session token is required.")
			return
		}
		h(w, r)
	}
}

func qualificationJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 24<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		jsonStatusError(w, 400, "Invalid readiness request. Check the JSON definition and size limits.")
		return false
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		jsonStatusError(w, 400, "Only one JSON request is allowed.")
		return false
	}
	return true
}

func qualificationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, qualification.ErrInvalid):
		jsonStatusError(w, 400, "Invalid definition. Review required categories, file checks and the offline artifact scope.")
	case errors.Is(err, qualification.ErrNotFound):
		jsonStatusError(w, 404, "No such responsibility or evaluation.")
	case errors.Is(err, qualification.ErrDenied):
		jsonStatusError(w, 409, "This revision requires evaluation and owner authorization before it can run.")
	case errors.Is(err, qualification.ErrConflict):
		jsonStatusError(w, 409, "A run is already active or this evaluation has ended.")
	default:
		jsonError(w, err)
	}
}

// readinessPageHandler renders a local owner console; evidence requires a session.
func (s *Server) readinessPageHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", 405)
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/readiness/"), "/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	found, err := s.Desktops.Get(r.Context(), id)
	if err != nil {
		jsonError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if err := s.Web.RenderReadiness(w, webui.ReadinessPageData{Desktop: found, OwnerFile: QualificationOwnerFile}); err != nil {
		jsonError(w, err)
	}
}

func (s *Server) qualificationListHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonStatusError(w, 405, "GET required")
		return
	}
	list, err := s.Qualification.List(r.URL.Query().Get("worker"))
	if err != nil {
		qualificationError(w, err)
		return
	}
	jsonResponse(w, list)
}

func (s *Server) qualificationSourceHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonStatusError(w, 405, "GET required")
		return
	}
	name := r.URL.Query().Get("name")
	if !playbookNamePattern.MatchString(name) {
		qualificationError(w, qualification.ErrInvalid)
		return
	}
	script, language, err := s.qualificationSource(r.Context(), r.URL.Query().Get("worker"), name)
	if err != nil {
		jsonStatusError(w, 422, "Update the worker's agent and choose a saved Python or Bash tool.")
		return
	}
	jsonResponse(w, map[string]string{"script": script, "language": language})
}

// qualificationDefineHandler pins the current tool bytes and actual image.
// A form cannot invent a fingerprint for an executable it has never exported.
func (s *Server) qualificationDefineHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonStatusError(w, 405, "POST required")
		return
	}
	var body struct {
		ID         string                   `json:"id"`
		Definition qualification.Definition `json:"definition"`
	}
	if !qualificationJSON(w, r, &body) {
		return
	}
	d := body.Definition
	if !playbookNamePattern.MatchString(d.SourceName) || d.WorkerID == "" {
		qualificationError(w, qualification.ErrInvalid)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	script, language, err := s.qualificationSource(ctx, d.WorkerID, d.SourceName)
	if err != nil {
		jsonStatusError(w, 422, "Start the worker and choose a saved Python or Bash tool from its playbooks.")
		return
	}
	d.Script, d.Language, d.Runtime = script, language, qualification.RuntimeVersion
	if s.Desktops == nil {
		jsonStatusError(w, 503, "The isolated runner is unavailable.")
		return
	}
	d.Image, err = s.Desktops.QualificationImage(ctx, d.WorkerID)
	if err != nil {
		jsonStatusError(w, 503, "Could not pin the worker's local container image.")
		return
	}
	if d.TimeoutSeconds == 0 {
		d.TimeoutSeconds = 60
	}
	resp, err := s.Qualification.Save(body.ID, d)
	if err != nil {
		qualificationError(w, err)
		return
	}
	jsonResponse(w, resp)
}

func (s *Server) qualificationSource(ctx context.Context, worker, name string) (string, string, error) {
	var resp *http.Response
	var err error
	if s.agentRequest != nil {
		resp, err = s.agentRequest(ctx, worker, http.MethodGet, "/playbooks/"+name+"/qualification-source", nil)
	} else if s.Desktops != nil {
		resp, err = s.Desktops.AgentRequest(ctx, worker, http.MethodGet, "/playbooks/"+name+"/qualification-source", nil)
	} else {
		return "", "", qualification.ErrDenied
	}
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", qualification.ErrDenied
	}
	var payload struct {
		Script   string `json:"script"`
		Language string `json:"language"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 256<<10)).Decode(&payload); err != nil {
		return "", "", err
	}
	if payload.Script == "" || len(payload.Script) > 128<<10 || (payload.Language != "python" && payload.Language != "bash") {
		return "", "", qualification.ErrDenied
	}
	return payload.Script, payload.Language, nil
}

func (s *Server) qualificationCurrent(ctx context.Context, r qualification.Responsibility) error {
	script, language, err := s.qualificationSource(ctx, r.Definition.WorkerID, r.Definition.SourceName)
	if err != nil {
		return err
	}
	image, err := s.Desktops.QualificationImage(ctx, r.Definition.WorkerID)
	if err != nil {
		return err
	}
	if script != r.Definition.Script || language != r.Definition.Language || image != r.Definition.Image || r.Definition.Runtime != qualification.RuntimeVersion {
		if err := s.Qualification.Invalidate(r.ID, "The saved tool or container image changed; revise the definition and retest.", false); err != nil {
			return err
		}
		return qualification.ErrDenied
	}
	return nil
}

func (s *Server) qualificationAuthorizeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonStatusError(w, 405, "POST required")
		return
	}
	var body struct {
		ID       string `json:"id"`
		Revision string `json:"revision"`
		Action   string `json:"action"`
	}
	if !qualificationJSON(w, r, &body) {
		return
	}
	if body.Action == "enable" {
		resp, err := s.Qualification.Get(body.ID)
		if err != nil {
			qualificationError(w, err)
			return
		}
		if err = s.qualificationCurrent(r.Context(), resp); err != nil {
			jsonStatusError(w, 409, "Could not confirm the evaluated tool and image. Start the worker, or revise and retest.")
			return
		}
	}
	resp, err := s.Qualification.Authorize(body.ID, body.Revision, body.Action)
	if err != nil {
		qualificationError(w, err)
		return
	}
	jsonResponse(w, resp)
}

func (s *Server) qualificationRunHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonStatusError(w, 405, "POST required")
		return
	}
	var body struct {
		ID       string              `json:"id"`
		Revision string              `json:"revision"`
		Mode     string              `json:"mode"`
		Case     *qualification.Case `json:"case,omitempty"`
	}
	if !qualificationJSON(w, r, &body) {
		return
	}
	resp, err := s.Qualification.Get(body.ID)
	if err != nil {
		qualificationError(w, err)
		return
	}
	if s.Desktops == nil || s.Tasks == nil {
		jsonStatusError(w, 503, "The isolated runner is unavailable.")
		return
	}
	if body.Mode == "production" {
		if body.Case == nil || body.Case.Category != "production" || qualification.ValidateCase(*body.Case) != nil {
			qualificationError(w, qualification.ErrInvalid)
			return
		}
	} else if body.Mode != "practice" || body.Case != nil {
		qualificationError(w, qualification.ErrInvalid)
		return
	}
	if err = s.qualificationCurrent(r.Context(), resp); err != nil {
		jsonStatusError(w, 409, "Could not confirm the evaluated tool and image. Start the worker, or revise and retest.")
		return
	}
	// Separate task identity keeps evaluation traffic out of a live desktop's
	// attribution. The program itself has no network and cannot report outcomes.
	s.qualificationRuns.mu.Lock()
	defer s.qualificationRuns.mu.Unlock()
	if len(s.qualificationRuns.active) >= 2 {
		jsonStatusError(w, 409, "Two isolated runs are already active. Wait or cancel one before starting another.")
		return
	}
	route := tasks.NewRoute()
	run, err := s.Qualification.BeginExecution(body.ID, body.Revision, body.Mode, route.TaskID, body.Case)
	if err != nil {
		qualificationError(w, err)
		return
	}
	worker := tasks.Worker{ID: "readiness-" + run.ID, Name: resp.Definition.Name, Engine: "qualified-tool", Profile: "offline-artifacts"}
	taskType := tasks.TypeFiles
	if body.Mode == "practice" {
		taskType = tasks.TypePractice
	}
	_, err = s.Tasks.CommitTyped(route, worker, "Readiness "+body.Mode+": "+resp.Definition.Name, taskType)
	if err != nil {
		_, _ = s.Qualification.FinishRun(run.ID, nil, nil)
		jsonError(w, err)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	if s.qualificationRuns.active == nil {
		s.qualificationRuns.active = map[string]context.CancelFunc{}
	}
	s.qualificationRuns.active[run.ID] = cancel
	go s.executeQualification(ctx, run, worker.ID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	jsonResponse(w, run)
}

// executeQualification runs every holdout afresh, recording evidence outside Docker.
func (s *Server) executeQualification(ctx context.Context, run qualification.Run, workerID string) {
	defer func() {
		s.qualificationRuns.mu.Lock()
		cancel := s.qualificationRuns.active[run.ID]
		delete(s.qualificationRuns.active, run.ID)
		s.qualificationRuns.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	}()
	cases := run.Definition.Cases
	if run.ProductionCase != nil {
		cases = []qualification.Case{*run.ProductionCase}
	}
	findings := make([]qualification.Finding, 0, len(cases))
	artifacts := map[string][]byte{}
	for _, c := range cases {
		if ctx.Err() != nil {
			break
		}
		result, err := s.Desktops.RunQualification(ctx, run.Definition, c)
		f := qualification.Verify(c, result, err)
		if recordErr := s.Qualification.RecordCase(run.ID, f, result.Artifacts); recordErr != nil {
			f.Passed = false
			f.Problems = append(f.Problems, "Could not persist complete case evidence; the run cannot qualify.")
			findings = append(findings, f)
			break
		}
		findings = append(findings, f)
		// Failed production bytes remain reviewable but never count as a success.
		if run.ProductionCase != nil && err == nil {
			artifacts = result.Artifacts
		}
	}
	finished, err := s.Qualification.FinishRun(run.ID, findings, artifacts)
	kind, reply := tasks.OutcomeError, "Independent readiness check failed. Review the evaluation evidence."
	if err == nil && finished.Status == "passed" {
		kind, reply = tasks.OutcomeDone, "Independent readiness check passed. Evidence: "+run.ID
	}
	checked := err == nil
	if err == nil && finished.Status == "passed" && run.ProductionCase != nil && run.ProductionCase.Expected != "complete" {
		stopKind := tasks.OutcomeQuery
		if run.ProductionCase.Expected == "approval" {
			stopKind = tasks.OutcomeApproval
		}
		reply = "The tool stopped without producing artifacts, as independently expected. No approval was granted and no external action was taken. Evidence: " + run.ID
		_, _ = s.Tasks.RecordOutcome(workerID, run.TaskID, tasks.Outcome{Kind: stopKind, Reply: reply, Engine: "qualified-tool", Checked: &checked})
	}
	_, _ = s.Tasks.RecordOutcome(workerID, run.TaskID, tasks.Outcome{Kind: kind, Reply: reply, Engine: "qualified-tool", Checked: &checked})
}

func (s *Server) qualificationCancelHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonStatusError(w, 405, "POST required")
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if !qualificationJSON(w, r, &body) {
		return
	}
	run, err := s.Qualification.Cancel(body.ID)
	if err != nil {
		qualificationError(w, err)
		return
	}
	s.qualificationRuns.mu.Lock()
	cancel := s.qualificationRuns.active[run.ID]
	s.qualificationRuns.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if s.Tasks != nil {
		if err := s.Tasks.CloseOpen("readiness-"+run.ID, "Owner cancelled the readiness run."); err != nil {
			jsonError(w, err)
			return
		}
	}
	jsonResponse(w, run)
}

func (s *Server) qualificationEvidenceHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonStatusError(w, 405, "GET required")
		return
	}
	run, err := s.Qualification.Run(r.URL.Query().Get("id"))
	if err != nil {
		qualificationError(w, err)
		return
	}
	jsonResponse(w, run)
}

func (s *Server) qualificationHistoryHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonStatusError(w, 405, "GET required")
		return
	}
	runs, err := s.Qualification.History(r.URL.Query().Get("id"))
	if err != nil {
		qualificationError(w, err)
		return
	}
	// Listing needs identities and decisions, not another copy of every fixture.
	for i := range runs {
		runs[i].Definition = qualification.Definition{}
		runs[i].ProductionCase = nil
		runs[i].Artifacts = nil
		runs[i].CaseArtifacts = nil
	}
	jsonResponse(w, runs)
}

func (s *Server) qualificationArtifactHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonStatusError(w, 405, "GET required")
		return
	}
	name := r.URL.Query().Get("file")
	if !qualification.FileName(name) {
		qualificationError(w, qualification.ErrInvalid)
		return
	}
	run, err := s.Qualification.Run(r.URL.Query().Get("id"))
	if err != nil {
		qualificationError(w, err)
		return
	}
	b, ok := run.Artifacts[name]
	if caseID := r.URL.Query().Get("case"); caseID != "" {
		b, ok = run.CaseArtifacts[caseID][name]
	}
	if !ok {
		jsonStatusError(w, 404, "No such saved artifact.")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+strings.ReplaceAll(name, "\"", "")+"\"")
	_, _ = w.Write(b)
}
