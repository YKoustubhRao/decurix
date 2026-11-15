// training_export.go serves the checked-done runs as a training set (internal/sftexport): a person's, at this machine.

package httpapi

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/runs"
	"github.com/YKoustubhRao/decurix/internal/runsearch"
	"github.com/YKoustubhRao/decurix/internal/sftexport"
)

// trainingSource reads the export's runs from this server's task records and run records.
type trainingSource struct{ s *Server }

// Ended lists every run that ended since since, oldest first.
func (t trainingSource) Ended(since time.Time) ([]runs.ExecutionRef, error) {
	keys, err := t.s.Tasks.EndedExecutions("", since)
	if err != nil {
		return nil, err
	}
	out := make([]runs.ExecutionRef, len(keys))
	for i, k := range keys {
		out[i] = runs.ExecutionRef{ExecID: k.ExecID, TaskID: k.TaskID, Worker: k.WorkerID}
	}
	return out, nil
}

// Trajectory builds one run's record.
func (t trainingSource) Trajectory(ctx context.Context, execID string) (runs.Trajectory, error) {
	return t.s.trajectoryOf(ctx, execID)
}

// FinalReply is the final report of one execution, as the task records keep it.
func (t trainingSource) FinalReply(taskID, execID string) (string, error) {
	d, err := t.s.Tasks.Get(taskID)
	if err != nil {
		return "", err
	}
	for _, e := range d.Executions {
		if e.ID == execID {
			return e.FinalReply, nil
		}
	}
	return "", nil
}

// trainingExportHandler answers GET /api/runs/training-export?since=30d[&format=decurix|toolcall][&worker=][&max=]:
// one JSON line per run that ended since then and whose verdict is a checked done (sftexport.Eligible), as a download.
// What it skipped, and why, is logged and sent as the X-Decurix-Skipped trailer.
func (s *Server) trainingExportHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	if s.RunStore == nil && s.buildRun == nil {
		jsonStatusError(w, http.StatusServiceUnavailable, "run records are not kept here")
		return
	}
	q := r.URL.Query()
	since, err := runsearch.ParseBound(orDefaultString(q.Get("since"), "30d"), time.Now().UTC())
	if err != nil {
		jsonStatusError(w, http.StatusBadRequest, "since: a date (2026-07-01), a time (RFC 3339) or a span back from now (24h, 7d, 2w)")
		return
	}
	format := strings.TrimSpace(q.Get("format"))
	if format == "" {
		format = sftexport.FormatDecurix
	}
	if format != sftexport.FormatDecurix && format != sftexport.FormatToolCall {
		jsonStatusError(w, http.StatusBadRequest, "format is decurix or toolcall")
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", `attachment; filename="decurix-training-`+format+`.jsonl"`)
	w.Header().Set("Trailer", "X-Decurix-Exported, X-Decurix-Skipped")
	sum, err := sftexport.Write(r.Context(), w, trainingSource{s}, sftexport.Options{Since: since, Format: format,
		Worker: strings.TrimSpace(q.Get("worker")), Max: atoiOr(q.Get("max"), 0)})
	if err != nil {
		log.Printf("training export: %v", err)
	}
	var skipped []string
	for why, n := range sum.Skipped {
		skipped = append(skipped, why+"="+strconv.Itoa(n))
	}
	w.Header().Set("X-Decurix-Exported", strconv.Itoa(sum.Exported))
	w.Header().Set("X-Decurix-Skipped", strings.Join(skipped, "; "))
	log.Printf("training export (%s, since %s): %d of %d runs written; skipped %v", format, since.Format(time.RFC3339),
		sum.Exported, sum.Runs, sum.Skipped)
}

// orDefaultString is s, or def when s is blank.
func orDefaultString(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
