package httpapi

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/YKoustubhRao/decurix/internal/blobstore"
	"github.com/YKoustubhRao/decurix/internal/llmrec"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// The recorded model calls (plan R2) are read here: by a person at this machine only (primary listener, humansOnly),
// since the stored messages are a worker's whole conversations, withheld but still the firm's work.

// llmCallsHandler lists the calls recorded for a task (?task=), without bodies: purpose, turn, deployment, usage,
// latency, status and the hashes of the messages and answer.
func (s *Server) llmCallsHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	if s.LLMRec == nil {
		jsonStatusError(w, http.StatusServiceUnavailable, "model calls are not recorded here")
		return
	}
	task := strings.TrimSpace(r.URL.Query().Get("task"))
	if !tasks.ValidID(task) && !llmrec.ValidTask(task) {
		jsonStatusError(w, http.StatusBadRequest, "name the task: ?task=<id>")
		return
	}
	rows, err := s.LLMRec.Calls(task)
	if err != nil {
		internalError(w, "llm calls", err)
		return
	}
	jsonResponse(w, map[string]any{"task_id": task, "calls": rows})
}

// llmCallStatsHandler counts the recorded calls by purpose, and the untagged, invalid and unrecorded ones.
func (s *Server) llmCallStatsHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	if s.LLMRec == nil {
		jsonStatusError(w, http.StatusServiceUnavailable, "model calls are not recorded here")
		return
	}
	st, err := s.LLMRec.Stats()
	if err != nil {
		// The count of calls that could not be recorded is the one thing still known when the log cannot be read,
		// and the one most worth seeing then.
		log.Printf("llm call stats: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "the call log could not be read",
			"record_failures": s.LLMRec.Failures()})
		return
	}
	jsonResponse(w, st)
}

// llmBlobHandler serves one stored message, answer, tool list or image by its SHA-256 (/api/llm/blobs/<sha>).
func (s *Server) llmBlobHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	if s.LLMRec == nil {
		jsonStatusError(w, http.StatusServiceUnavailable, "model calls are not recorded here")
		return
	}
	sha := strings.TrimPrefix(r.URL.Path, "/api/llm/blobs/")
	if !blobstore.ValidSHA(sha) {
		jsonStatusError(w, http.StatusBadRequest, "not a sha256")
		return
	}
	rc, err := s.LLMRec.OpenBlob(sha)
	if err != nil {
		jsonStatusError(w, http.StatusNotFound, "no such blob")
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", "attachment; filename="+sha)
	_, _ = io.Copy(w, rc)
}
