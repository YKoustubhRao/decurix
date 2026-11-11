package httpapi

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/eval"
	"github.com/YKoustubhRao/decurix/internal/practice"
)

// The eval runner's routes (docs/evals.md): start a matrix, follow it, read its rows and table, cancel it. They are
// host-only: registered on the primary listener alone (never the gated :8090) and refused from inside a desktop.

// maxEvalUpload bounds a matrix upload as sent: the suite tar and the arms' agent trees together, each gzip-compressed
// or not. It is in bodyLimits: under the default 1 MiB outer cap an agent tree (3.5 MB) was cut off mid-upload (F-ENV-1).
const maxEvalUpload = 256 << 20

// evalRunsPath is the route that starts a matrix (and lists runs).
const evalRunsPath = "/api/evals/runs"

// maxAgentTree bounds one arm's agent tree once unpacked.
const maxAgentTree = 64 << 20

type evalRoutes struct {
	e *eval.Service
}

// RegisterEvalRoutes registers the eval routes on mux, which must be the primary listener's (C13).
func (s *Server) RegisterEvalRoutes(mux *http.ServeMux, e *eval.Service) {
	h := &evalRoutes{e: e}
	mux.HandleFunc(evalRunsPath, s.humansOnly(h.runs))
	mux.HandleFunc("/api/evals/runs/", s.humansOnly(h.run))
}

// ready answers 503 when the eval runner is not set up.
func (h *evalRoutes) ready(w http.ResponseWriter) bool {
	if h.e == nil || h.e.Store == nil {
		jsonStatusError(w, http.StatusServiceUnavailable, "the eval runner is not set up")
		return false
	}
	return true
}

// runs lists runs (GET) or starts one (POST, multipart: suite, arms, repeats, cases, carry_state, from_worker, tree_<arm>).
func (h *evalRoutes) runs(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		list, err := h.e.Store.List(limit)
		if err != nil {
			internalError(w, "eval: list", err)
			return
		}
		if list == nil {
			list = []eval.Run{}
		}
		jsonResponse(w, map[string]any{"runs": list})
	case http.MethodPost:
		h.start(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		jsonStatusError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (h *evalRoutes) start(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxEvalUpload)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		badForm(w, err)
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	suite, sent, ok := evalFile(r, "suite", eval.MaxSuiteBytes)
	if !sent {
		jsonStatusError(w, http.StatusBadRequest, "a suite tar is needed (field \"suite\")")
		return
	}
	if !ok {
		jsonStatusError(w, http.StatusBadRequest, "the suite: "+unreadableUpload)
		return
	}
	var arms []eval.Arm
	if err := json.Unmarshal([]byte(r.FormValue("arms")), &arms); err != nil {
		jsonStatusError(w, http.StatusBadRequest, "arms must be a JSON list")
		return
	}
	for i := range arms {
		tree, sent, ok := evalFile(r, "tree_"+arms[i].Name, maxAgentTree)
		switch {
		case !sent:
			arms[i].AgentTree, arms[i].AgentTreeRev = nil, ""
		case !ok:
			// A tree that was sent and cannot be read is refused, never run as the image's code under the arm's name.
			jsonStatusError(w, http.StatusBadRequest, fmt.Sprintf("the agent tree of arm %s: %s", clipArmName(arms[i].Name), unreadableUpload))
			return
		default:
			arms[i].AgentTree = tree
		}
	}
	repeats, err := strconv.Atoi(strings.TrimSpace(r.FormValue("repeats")))
	if err != nil {
		jsonStatusError(w, http.StatusBadRequest, "repeats must be a whole number")
		return
	}
	var cases []string
	for _, c := range strings.Split(r.FormValue("cases"), ",") {
		if c = strings.TrimSpace(c); c != "" {
			cases = append(cases, c)
		}
	}
	// carry_state asks for the mode that keeps the desktop's state from case to case; the suite must say so too.
	carry := strings.TrimSpace(r.FormValue("carry_state"))
	// from_worker starts every case from that desktop's taught state (and its agent tree): teach once, then measure.
	from := strings.TrimSpace(r.FormValue("from_worker"))
	if from != "" && !eval.ValidWorkerID(from) {
		jsonStatusError(w, http.StatusBadRequest, "from_worker must be a desktop's id (not a practice or readiness desktop)")
		return
	}
	run, err := h.e.Start(r.Context(), eval.MatrixRequest{SuiteTar: suite, Arms: arms, Repeats: repeats, Cases: cases,
		CarryState: carry == "1" || carry == "true", FromWorker: from})
	if err != nil {
		h.refuse(w, err)
		return
	}
	jsonResponseStatus(w, http.StatusAccepted, map[string]any{"run": run})
}

// refuse answers a matrix that did not start: the suite's problems and the runner's own sentences are the caller's
// to read; anything else is an internal error.
func (h *evalRoutes) refuse(w http.ResponseWriter, err error) {
	var problems eval.Problems
	var bad eval.BadRequest
	var low practice.LowMemoryError
	switch {
	case errors.As(err, &problems):
		jsonResponseStatus(w, http.StatusBadRequest, map[string]any{"error": "the suite has problems", "problems": refusalLines(problems)})
	case errors.As(err, &bad):
		jsonStatusError(w, http.StatusBadRequest, refusal(bad))
	case errors.Is(err, eval.ErrBusy):
		jsonStatusError(w, http.StatusConflict, refusal(eval.ErrBusy))
	case errors.As(err, &low):
		jsonStatusError(w, http.StatusConflict, refusal(low))
	default:
		internalError(w, "eval: start", err)
	}
}

// unreadableUpload is said of an uploaded tar that could not be read.
const unreadableUpload = "empty, not a gzip or tar that can be read, or too large unpacked"

// evalFile reads one uploaded tar, gunzipped first when it was sent gzip-compressed (known by its magic bytes). The
// limit holds after decompression, so a small upload cannot expand past it. sent is false when the field is absent,
// ok false when it was sent and could not be read.
func evalFile(r *http.Request, field string, limit int64) (b []byte, sent, ok bool) {
	if r.MultipartForm == nil || len(r.MultipartForm.File[field]) == 0 {
		return nil, false, false
	}
	b, ok = formFile(r, field, limit)
	if !ok {
		return nil, true, false
	}
	if len(b) < 2 || b[0] != 0x1f || b[1] != 0x8b {
		return b, true, true
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, true, false
	}
	defer zr.Close()
	out, err := io.ReadAll(io.LimitReader(zr, limit+1))
	if err != nil || int64(len(out)) > limit || len(out) == 0 {
		return nil, true, false
	}
	return out, true, true
}

// ---- teaching a case on a normal desktop ----

// evalTeachPath sets a normal desktop up to record a demonstration of one case of a suite (POST); evalTeachStopPath
// undoes it.
const (
	evalTeachPath     = "/api/evals/teach"
	evalTeachStopPath = "/api/evals/teach/stop"
)

// maxEvalTeachUpload bounds a teach upload as sent: one suite tar, gzip-compressed or not.
const maxEvalTeachUpload = eval.MaxSuiteBytes + 1<<20

// Where a teach desktop gets its case: the case's inputs in the folder a practice desktop gives them, and the case's
// mock app run as the worker on the desktop's loopback, which the suite's host names are mapped to.
const (
	teachMockPort    = 18080
	teachMockDir     = "/home/headless/work/teach-mock"
	teachCaseRoot    = "/home/headless/work/practice"
	teachMockSeconds = 20
)

// TeachDesktops is what teaching a case needs of a normal desktop (*desktop.Manager).
type TeachDesktops interface {
	CopyFiles(ctx context.Context, desktopID, dir string, files []desktop.File) error
	StartWorkerProcess(ctx context.Context, id, dir string, argv []string, waitPort, waitSeconds int) error
	StopWorkerProcess(ctx context.Context, id, dir string) error
	MapBrowserHosts(ctx context.Context, id string, hosts []string, target string) error
	UnmapBrowserHosts(ctx context.Context, id string) error
}

type teachRoutes struct {
	d TeachDesktops
}

// RegisterEvalTeachRoutes registers the routes that set a normal desktop up to record a demonstration of a suite's
// case on the suite's own host names (docs/evals.md, "Teaching once"), on mux, which must be the primary listener's:
// they start a process in a desktop and change its browser's flags.
func (s *Server) RegisterEvalTeachRoutes(mux *http.ServeMux, d TeachDesktops) {
	h := &teachRoutes{d: d}
	mux.HandleFunc(evalTeachPath, s.humansOnly(h.teach))
	mux.HandleFunc(evalTeachStopPath, s.humansOnly(h.stop))
}

// teachDesktopID reads and checks the desktop a teach route names: a normal desktop's id.
func teachDesktopID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := strings.TrimSpace(r.FormValue("desktop"))
	if !eval.ValidWorkerID(id) {
		jsonStatusError(w, http.StatusBadRequest, "desktop must be a normal desktop's id (not a practice or readiness desktop)")
		return "", false
	}
	return id, true
}

// teachFailed answers a desktop-side failure with a sentence of its own, never the error's text.
func teachFailed(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, desktop.ErrNotFound):
		jsonStatusError(w, http.StatusNotFound, "no such desktop")
	case errors.Is(err, desktop.ErrDesktopNotRunning):
		jsonStatusError(w, http.StatusConflict, "the desktop is not running: start or wake it first")
	case errors.Is(err, desktop.ErrNotTestHost):
		jsonStatusError(w, http.StatusBadRequest, "only host names under .test, .example, .invalid or .localhost can be mapped in a desktop's browser")
	default:
		internalError(w, "eval: teach: "+what, err)
	}
}

// teach (POST, multipart: suite, case, desktop) puts the case's inputs into ~/work/practice/<case>/ on the desktop,
// starts the case's mock there with its fixture, as the worker, on 127.0.0.1:teachMockPort, and maps the suite's
// hosts to it in the desktop's browser from its next launch.
func (h *teachRoutes) teach(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		jsonStatusError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if h.d == nil {
		jsonStatusError(w, http.StatusServiceUnavailable, "teaching is not set up")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxEvalTeachUpload)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		badForm(w, err)
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	id, ok := teachDesktopID(w, r)
	if !ok {
		return
	}
	tar, sent, ok := evalFile(r, "suite", eval.MaxSuiteBytes)
	if !sent || !ok {
		jsonStatusError(w, http.StatusBadRequest, "a suite tar is needed (field \"suite\"): "+unreadableUpload)
		return
	}
	suite, err := eval.Load(tar)
	if err != nil {
		var problems eval.Problems
		if errors.As(err, &problems) {
			jsonResponseStatus(w, http.StatusBadRequest, map[string]any{"error": "the suite has problems", "problems": refusalLines(problems)})
			return
		}
		jsonStatusError(w, http.StatusBadRequest, "the suite could not be read")
		return
	}
	c, found := suite.Case(strings.TrimSpace(r.FormValue("case")))
	if !found {
		jsonStatusError(w, http.StatusBadRequest, "the suite has no such case")
		return
	}
	var hosts []string
	if c.Start.Mock != "" {
		hosts = suite.Mocks[c.Start.Mock].Hosts
		for _, host := range hosts {
			if !desktop.ReservedTestHost(host) {
				teachFailed(w, "hosts", desktop.ErrNotTestHost)
				return
			}
		}
	}
	ctx := r.Context()
	folder := teachCaseRoot + "/" + c.ID
	inputs := make([]desktop.File, 0, len(c.Files))
	for _, name := range c.Start.Files {
		inputs = append(inputs, desktop.File{Name: name, Data: c.Files[name]})
	}
	if len(inputs) > 0 {
		if err := h.d.CopyFiles(ctx, id, folder, inputs); err != nil {
			teachFailed(w, "copy the inputs", err)
			return
		}
	}
	answer := map[string]any{"desktop": id, "case": c.ID, "folder": folder, "hosts": []string{}, "port": 0}
	if c.Start.Mock == "" {
		answer["say"] = "The case's inputs are in " + folder + ". The case has no app to map."
		jsonResponse(w, answer)
		return
	}
	fixture := c.Fixture
	if len(fixture) == 0 {
		fixture = []byte("{}")
	}
	if err := h.d.CopyFiles(ctx, id, teachMockDir, []desktop.File{{Name: "mock_app.py", Data: suite.MockSources[c.Start.Mock]},
		{Name: "fixture.json", Data: fixture}}); err != nil {
		teachFailed(w, "copy the mock", err)
		return
	}
	port := fmt.Sprint(teachMockPort)
	argv := []string{"/usr/bin/python3", "-I", "mock_app.py", "--host", "127.0.0.1", "--port", port,
		"--fixture", "fixture.json", "--state", "state.json"}
	if err := h.d.StartWorkerProcess(ctx, id, teachMockDir, argv, teachMockPort, teachMockSeconds); err != nil {
		teachFailed(w, "start the mock", err)
		return
	}
	if err := h.d.MapBrowserHosts(ctx, id, hosts, "127.0.0.1:"+port); err != nil {
		teachFailed(w, "map the hosts", err)
		return
	}
	answer["hosts"], answer["port"] = hosts, teachMockPort
	answer["say"] = fmt.Sprintf("The case's inputs are in %s and its app answers at %s in this desktop's browser. Close the "+
		"browser if it is open, then open it again so it takes the new host names.", folder, strings.Join(hosts, ", "))
	jsonResponse(w, answer)
}

// stop (POST, form: desktop) stops the teach mock and takes the host mapping out of the desktop's browser flags.
func (h *teachRoutes) stop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		jsonStatusError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if h.d == nil {
		jsonStatusError(w, http.StatusServiceUnavailable, "teaching is not set up")
		return
	}
	id, ok := teachDesktopID(w, r)
	if !ok {
		return
	}
	if err := h.d.StopWorkerProcess(r.Context(), id, teachMockDir); err != nil {
		teachFailed(w, "stop the mock", err)
		return
	}
	if err := h.d.UnmapBrowserHosts(r.Context(), id); err != nil {
		teachFailed(w, "unmap the hosts", err)
		return
	}
	jsonResponse(w, map[string]any{"desktop": id, "say": "The case's app is stopped and its host names are no longer mapped. " +
		"Close the browser if it is open."})
}

// clipArmName keeps an arm's name short in a refusal (checkArms validates it later).
func clipArmName(name string) string {
	if len(name) > 64 {
		return name[:64]
	}
	return name
}

// formFile reads one uploaded file of at most limit bytes.
func formFile(r *http.Request, field string, limit int64) ([]byte, bool) {
	f, _, err := r.FormFile(field)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit || len(b) == 0 {
		return nil, false
	}
	return b, true
}

// run serves one run: GET <id> (status, progress, current case), GET <id>/rows, GET <id>/table?format=md|json,
// POST <id>/cancel.
func (h *evalRoutes) run(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/evals/runs/"), "/")
	id, sub, _ := strings.Cut(rest, "/")
	if id == "" || strings.Contains(sub, "/") {
		http.NotFound(w, r)
		return
	}
	run, err := h.e.Store.Get(id)
	if errors.Is(err, eval.ErrNotFound) {
		jsonStatusError(w, http.StatusNotFound, "no such run")
		return
	}
	if err != nil {
		internalError(w, "eval: run", err)
		return
	}
	switch {
	case sub == "" && r.Method == http.MethodGet:
		progress, running := h.e.Progress(id)
		jsonResponse(w, map[string]any{"run": run, "progress": progress, "active": running})
	case sub == "rows" && r.Method == http.MethodGet:
		rows, err := h.e.Store.Rows(id)
		if err != nil {
			internalError(w, "eval: rows", err)
			return
		}
		jsonResponse(w, map[string]any{"rows": rows})
	case sub == "table" && r.Method == http.MethodGet:
		rows, err := h.e.Store.Rows(id)
		if err != nil {
			internalError(w, "eval: table", err)
			return
		}
		t := eval.Summarise(run, rows)
		if r.URL.Query().Get("format") == "json" {
			jsonResponse(w, t)
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = io.WriteString(w, t.Markdown())
	case sub == "cancel" && r.Method == http.MethodPost:
		run, err := h.e.Cancel(id)
		if err != nil {
			internalError(w, "eval: cancel", err)
			return
		}
		jsonResponse(w, map[string]any{"run": run})
	default:
		jsonStatusError(w, http.StatusNotFound, "not found")
	}
}
