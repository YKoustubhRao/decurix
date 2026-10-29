package skillhub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxRequestBytes bounds a request body sent to these routes; none carries a bundle.
const maxRequestBytes = 64 << 10

// defaultAgentTimeout bounds one call to a desktop's agent.
const defaultAgentTimeout = 60 * time.Second

// AgentCaller reaches a desktop's own agent API. *desktop.Manager satisfies it
// with its AgentRequest, so the handler is wired with no adapter; tests use a
// fake. A nil body is a request without one; otherwise it is JSON.
type AgentCaller interface {
	AgentRequest(ctx context.Context, worker, method, path string, body io.Reader) (*http.Response, error)
}

// Handler serves the registry's routes.
type Handler struct {
	Store *Store
	Agent AgentCaller
	// Timeout bounds one call to a desktop's agent; zero means a minute.
	Timeout time.Duration
}

// Register adds the registry's routes to mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/skillhub", h.list)
	mux.HandleFunc("/api/skillhub/publish", h.publish)
	mux.HandleFunc("/api/skillhub/install", h.install)
	mux.HandleFunc("/api/skillhub/roles", h.roles)
	mux.HandleFunc("/api/skillhub/assign", h.assign)
	mux.HandleFunc("/api/skillhub/sync", h.sync)
	mux.HandleFunc("/api/skillhub/outdated", h.outdated)
}

// request is every field the POST routes take; each route reads its own.
type request struct {
	Worker  string   `json:"worker"`
	Name    string   `json:"name"`
	Note    string   `json:"note"`
	Version int      `json:"version"`
	Role    string   `json:"role"`
	Roles   []string `json:"roles"`
	Remove  bool     `json:"remove"`
	Replace bool     `json:"replace"`
}

// list answers GET /api/skillhub: every skill with its versions, roles and
// installs, and every worker's roles.
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodGet) {
		return
	}

	skills, err := h.Store.List()
	if err != nil {
		storeError(w, err)
		return
	}
	workers, err := h.Store.AllWorkerRoles()
	if err != nil {
		storeError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"skills": skills, "workers": workers})
}

// outdated answers GET /api/skillhub/outdated?worker=.
func (h *Handler) outdated(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodGet) {
		return
	}

	worker := strings.TrimSpace(r.URL.Query().Get("worker"))
	if !ValidWorker(worker) {
		writeError(w, http.StatusBadRequest, "missing or bad worker id")
		return
	}

	out, err := h.Store.Outdated(worker)
	if err != nil {
		storeError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"worker": worker, "outdated": out})
}

// publish answers POST /api/skillhub/publish {worker, name, note}: it reads
// the skill off the worker's desktop and stores it as a new version when it
// changed. Only a ready skill - one a person confirmed - is shared.
func (h *Handler) publish(w http.ResponseWriter, r *http.Request) {
	req, ok := h.decode(w, r)
	if !ok {
		return
	}
	if !ValidWorker(req.Worker) {
		writeError(w, http.StatusBadRequest, "missing or bad worker id")
		return
	}
	if !ValidName(req.Name) {
		writeError(w, http.StatusBadRequest, "not a skill name")
		return
	}

	status, body, err := h.call(r.Context(), req.Worker, http.MethodGet, "/skills/"+req.Name+"/bundle", nil, MaxBundleBytes)
	if err != nil {
		agentError(w, status, body, err)
		return
	}

	var head struct {
		Skill struct {
			Status string `json:"status"`
		} `json:"skill"`
	}
	if json.Unmarshal(body, &head) != nil {
		writeError(w, http.StatusBadGateway, "the desktop did not hand out a skill bundle")
		return
	}
	if head.Skill.Status != "ready" {
		writeError(w, http.StatusConflict, "only a skill a person has confirmed can be shared; finish teaching it first")
		return
	}

	v, created, err := h.Store.Publish(req.Name, body, req.Worker, req.Note)
	if err != nil {
		storeError(w, err)
		return
	}
	// The worker it came from holds exactly what was published.
	if err := h.Store.RecordInstall(req.Worker, v.Name, v.Version); err != nil {
		log.Printf("skillhub: publish %s: %v", v.Name, err)
	}

	writeJSON(w, http.StatusOK, map[string]any{"version": v, "created": created})
}

// install answers POST /api/skillhub/install {worker, name, version?, replace?}.
func (h *Handler) install(w http.ResponseWriter, r *http.Request) {
	req, ok := h.decode(w, r)
	if !ok {
		return
	}
	if !ValidWorker(req.Worker) {
		writeError(w, http.StatusBadRequest, "missing or bad worker id")
		return
	}
	if !ValidName(req.Name) || req.Version < 0 {
		writeError(w, http.StatusBadRequest, "not a skill name or version")
		return
	}

	summary, status, msg := h.installOne(r.Context(), req.Worker, req.Name, req.Version, req.Replace)
	if status != http.StatusOK {
		writeError(w, status, msg)
		return
	}

	writeJSON(w, http.StatusOK, summary)
}

// installResult is what one install answers: the version and the desktop's
// summary of the skill as it now holds it.
type installResult struct {
	Name    string          `json:"name"`
	Version int             `json:"version"`
	SHA     string          `json:"sha"`
	Skill   json.RawMessage `json:"skill,omitempty"`
}

// installOne pushes one version onto a worker's desktop and records it. On
// failure it returns the status and words to answer with.
func (h *Handler) installOne(ctx context.Context, worker, name string, version int, replace bool) (installResult, int, string) {
	v, err := h.Store.Get(name, version)
	if errors.Is(err, ErrNotFound) {
		return installResult{}, http.StatusNotFound, "no such skill version in the registry"
	}
	if err != nil {
		log.Printf("skillhub: install %s: %v", name, err)
		return installResult{}, http.StatusInternalServerError, "internal error"
	}

	payload, err := json.Marshal(map[string]any{
		"bundle":  v.Bundle,
		"version": v.Version,
		"sha":     v.SHA,
		"replace": replace,
	})
	if err != nil {
		log.Printf("skillhub: install %s: %v", name, err)
		return installResult{}, http.StatusInternalServerError, "internal error"
	}

	status, body, err := h.call(ctx, worker, http.MethodPost, "/skills/install", payload, maxRequestBytes)
	if err != nil {
		code, msg := agentFailure(status, body, err)
		return installResult{}, code, msg
	}

	if err := h.Store.RecordInstall(worker, v.Name, v.Version); err != nil {
		log.Printf("skillhub: install %s: %v", name, err)
		return installResult{}, http.StatusInternalServerError, "the skill was installed but not recorded"
	}

	result := installResult{Name: v.Name, Version: v.Version, SHA: v.SHA}
	if json.Valid(body) {
		result.Skill = body
	}

	return result, http.StatusOK, ""
}

// sync answers POST /api/skillhub/sync {worker}: it installs every skill
// Outdated reports, one by one, and says which went in and which did not. A
// skill the worker changed locally is not overwritten (the desktop refuses
// without replace), so it shows up under failed with the desktop's reason.
func (h *Handler) sync(w http.ResponseWriter, r *http.Request) {
	req, ok := h.decode(w, r)
	if !ok {
		return
	}
	if !ValidWorker(req.Worker) {
		writeError(w, http.StatusBadRequest, "missing or bad worker id")
		return
	}

	todo, err := h.Store.Outdated(req.Worker)
	if err != nil {
		storeError(w, err)
		return
	}

	type failure struct {
		Name  string `json:"name"`
		Error string `json:"error"`
	}
	installed := make([]installResult, 0)
	failed := make([]failure, 0)
	for _, item := range todo {
		res, status, msg := h.installOne(r.Context(), req.Worker, item.Name, item.Latest, false)
		if status != http.StatusOK {
			failed = append(failed, failure{Name: item.Name, Error: msg})
			continue
		}
		res.Skill = nil
		installed = append(installed, res)
	}

	writeJSON(w, http.StatusOK, map[string]any{"worker": req.Worker, "installed": installed, "failed": failed})
}

// roles answers POST /api/skillhub/roles {worker, roles[]}: it replaces the
// roles the worker carries.
func (h *Handler) roles(w http.ResponseWriter, r *http.Request) {
	req, ok := h.decode(w, r)
	if !ok {
		return
	}

	roles, err := h.Store.SetWorkerRoles(req.Worker, req.Roles)
	if err != nil {
		storeError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"worker": req.Worker, "roles": roles})
}

// assign answers POST /api/skillhub/assign {name, role, remove?}.
func (h *Handler) assign(w http.ResponseWriter, r *http.Request) {
	req, ok := h.decode(w, r)
	if !ok {
		return
	}

	var err error
	if req.Remove {
		err = h.Store.Unassign(req.Name, req.Role)
	} else {
		err = h.Store.Assign(req.Name, req.Role)
	}
	if err != nil {
		storeError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"name": req.Name, "role": strings.TrimSpace(req.Role), "assigned": !req.Remove})
}

// decode reads a POST's fields from a JSON body or, for a plain form, from
// its values (roles comma separated).
func (h *Handler) decode(w http.ResponseWriter, r *http.Request) (request, bool) {
	var req request
	if !method(w, r, http.MethodPost) {
		return req, false
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)

	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "the request body is not the JSON this route takes")
			return req, false
		}
	} else {
		if err := r.ParseForm(); err != nil {
			writeError(w, http.StatusBadRequest, "could not read the form")
			return req, false
		}
		req.Worker = r.PostFormValue("worker")
		req.Name = r.PostFormValue("name")
		req.Note = r.PostFormValue("note")
		req.Role = r.PostFormValue("role")
		req.Remove = r.PostFormValue("remove") == "true"
		req.Replace = r.PostFormValue("replace") == "true"
		if v := r.PostFormValue("version"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				writeError(w, http.StatusBadRequest, "version must be a number")
				return req, false
			}
			req.Version = n
		}
		for _, role := range strings.Split(r.PostFormValue("roles"), ",") {
			if role = strings.TrimSpace(role); role != "" {
				req.Roles = append(req.Roles, role)
			}
		}
	}

	req.Worker = strings.TrimSpace(req.Worker)
	req.Name = strings.TrimSpace(req.Name)

	return req, true
}

// errAgentStatus marks an answer from the desktop's agent that was not a 2xx.
var errAgentStatus = errors.New("desktop agent answered with an error")

// call makes one bounded call to a worker's agent and reads at most limit
// bytes of its answer. A non-2xx answer returns its status and body with
// errAgentStatus.
func (h *Handler) call(ctx context.Context, worker, verb, path string, payload []byte, limit int64) (int, []byte, error) {
	if h.Agent == nil {
		return 0, nil, errors.New("skillhub: no agent caller wired")
	}

	timeout := h.Timeout
	if timeout <= 0 {
		timeout = defaultAgentTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}

	resp, err := h.Agent.AgentRequest(ctx, worker, verb, path, body)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return 0, nil, err
	}
	if int64(len(data)) > limit {
		return resp.StatusCode, nil, fmt.Errorf("%w: answer larger than %d bytes", ErrInvalid, limit)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, data, errAgentStatus
	}

	return resp.StatusCode, data, nil
}

// agentFailure turns a failed agent call into a status and words to answer
// with. A 4xx from the agent carries words it wrote for a person and is passed
// on; anything else is logged and replaced, as jsonError's rule requires.
func agentFailure(status int, body []byte, err error) (int, string) {
	switch {
	case errors.Is(err, ErrInvalid):
		return http.StatusBadGateway, "the desktop's skill is too large to share"
	case errors.Is(err, errAgentStatus) && status >= 400 && status < 500:
		var said struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &said) == nil && said.Error != "" {
			return status, said.Error
		}
		return status, "the desktop refused that"
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "the desktop took too long to answer"
	default:
		log.Printf("skillhub: desktop agent: status %d: %v: %.500s", status, err, body)
		return http.StatusBadGateway, "the desktop could not be reached or could not do that"
	}
}

// agentError answers a failed agent call.
func agentError(w http.ResponseWriter, status int, body []byte, err error) {
	code, msg := agentFailure(status, body, err)
	writeError(w, code, msg)
}

// storeError answers a store error: refused input and unknown skills get the
// store's own words, anything else a generic line and a log entry.
func storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalid):
		writeError(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": "))
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "no such skill in the registry")
	default:
		log.Printf("skillhub: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// method answers 405 unless r uses want.
func method(w http.ResponseWriter, r *http.Request, want string) bool {
	if r.Method == want {
		return true
	}

	w.Header().Set("Allow", want)
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")

	return false
}

// writeJSON answers status with v as JSON.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError answers status with {"error": reason}.
func writeError(w http.ResponseWriter, status int, reason string) {
	writeJSON(w, status, map[string]string{"error": reason})
}
