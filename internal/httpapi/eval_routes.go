package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/YKoustubhRao/decurix/internal/eval"
	"github.com/YKoustubhRao/decurix/internal/practice"
)

// The eval runner's routes (docs/evals.md): start a matrix, follow it, read its rows and table, cancel it. They are
// host-only: registered on the primary listener alone (never the gated :8090) and refused from inside a desktop.

// maxEvalUpload bounds a matrix upload: the suite tar and the arms' agent trees together.
const maxEvalUpload = 256 << 20

type evalRoutes struct {
	e *eval.Service
}

// RegisterEvalRoutes registers the eval routes on mux, which must be the primary listener's (C13).
func (s *Server) RegisterEvalRoutes(mux *http.ServeMux, e *eval.Service) {
	h := &evalRoutes{e: e}
	mux.HandleFunc("/api/evals/runs", s.humansOnly(h.runs))
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
	suite, ok := formFile(r, "suite", eval.MaxSuiteBytes)
	if !ok {
		jsonStatusError(w, http.StatusBadRequest, "a suite tar is needed (field \"suite\")")
		return
	}
	var arms []eval.Arm
	if err := json.Unmarshal([]byte(r.FormValue("arms")), &arms); err != nil {
		jsonStatusError(w, http.StatusBadRequest, "arms must be a JSON list")
		return
	}
	for i := range arms {
		if tree, ok := formFile(r, "tree_"+arms[i].Name, 64<<20); ok {
			arms[i].AgentTree = tree
		} else {
			arms[i].AgentTree, arms[i].AgentTreeRev = nil, ""
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
	run, err := h.e.Start(r.Context(), eval.MatrixRequest{SuiteTar: suite, Arms: arms, Repeats: repeats, Cases: cases})
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
		jsonResponseStatus(w, http.StatusBadRequest, map[string]any{"error": "the suite has problems", "problems": []string(problems)})
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
