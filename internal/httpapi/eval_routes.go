package httpapi

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

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

// runs lists runs (GET) or starts one (POST, multipart: suite, arms, repeats, cases, tree_<arm>).
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
	run, err := h.e.Start(r.Context(), eval.MatrixRequest{SuiteTar: suite, Arms: arms, Repeats: repeats, Cases: cases,
		CarryState: carry == "1" || carry == "true"})
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
