package httpapi

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/runs"
)

// A run's record (plan R1): decurix.trajectory/v1 built by runs.Build from the task records, the call log, the
// judgements and the bundle, and the bundle itself as one tar with the trajectory beside it. A person's, at this
// machine (primary listener, humansOnly): it holds a worker's whole run.

// execIDPattern is the shape of an execution id a run route takes.
var execIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// runSources is what runs.Build joins, from this server's stores.
func (s *Server) runSources() runs.Sources {
	src := runs.Sources{Tasks: s.Tasks, Store: s.RunStore}
	if s.LLMRec != nil {
		src.Calls = s.LLMRec
	}
	if s.RunStore != nil {
		src.Blobs = s.RunStore.Blobs()
	}
	return src
}

// trajectoryOf builds execID's run record (buildRun in tests).
func (s *Server) trajectoryOf(ctx context.Context, execID string) (runs.Trajectory, error) {
	if s.buildRun != nil {
		return s.buildRun(ctx, execID)
	}
	return runs.Build(ctx, s.runSources(), execID)
}

// runsHandler serves /api/runs/<execution>/trajectory (the run record as JSON) and /api/runs/<execution>/bundle (a
// tar of the bundle's files at their paths, plus trajectory.json).
func (s *Server) runsHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	if s.RunStore == nil && s.buildRun == nil {
		jsonStatusError(w, http.StatusServiceUnavailable, "run records are not kept here")
		return
	}
	execID, what, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/api/runs/"), "/")
	if !execIDPattern.MatchString(execID) || (what != "trajectory" && what != "bundle") {
		http.NotFound(w, r)
		return
	}
	traj, err := s.trajectoryOf(r.Context(), execID)
	if errors.Is(err, runs.ErrUnknownExecution) {
		jsonStatusError(w, http.StatusNotFound, "no such run")
		return
	}
	if err != nil {
		internalError(w, "run record of "+execID, err)
		return
	}
	if what == "trajectory" {
		jsonResponse(w, traj)
		return
	}
	s.writeRunBundle(w, execID, traj)
}

// writeRunBundle writes the bundle's files and trajectory.json as one tar. A file whose blob cannot be read is left
// out and named in trajectory.json's integrity.missing, never sent cut.
func (s *Server) writeRunBundle(w http.ResponseWriter, execID string, traj runs.Trajectory) {
	var files []runs.File
	if s.RunStore != nil {
		b, ok, err := s.RunStore.Bundle(execID)
		if err != nil {
			internalError(w, "run bundle of "+execID, err)
			return
		}
		if ok {
			files = b.Files
		}
	}
	blobs := s.runSources().Blobs
	readable := files[:0:0]
	for _, f := range files {
		if blobs != nil && blobs.Has(f.SHA256) {
			readable = append(readable, f)
		} else {
			traj.Integrity.Missing = append(traj.Integrity.Missing, f.Path+": its stored copy cannot be read")
		}
	}
	doc, err := json.MarshalIndent(traj, "", "  ")
	if err != nil {
		internalError(w, "run record of "+execID, err)
		return
	}

	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Disposition", `attachment; filename="run-`+execID+`.tar"`)
	tw := tar.NewWriter(w)
	now := time.Now()
	if err := tw.WriteHeader(&tar.Header{Name: "trajectory.json", Mode: 0o644, Size: int64(len(doc)), ModTime: now}); err != nil {
		return
	}
	if _, err := tw.Write(doc); err != nil {
		return
	}
	for _, f := range readable {
		rc, err := blobs.Open(f.SHA256)
		if err != nil {
			log.Printf("run bundle %s: %s: %v", execID, f.Path, err)
			return // the tar is cut here: a client sees a broken archive, never a short file
		}
		err = func() error {
			defer rc.Close()
			if err := tw.WriteHeader(&tar.Header{Name: "home/" + strings.TrimPrefix(f.Path, "/"), Mode: 0o644, Size: f.Bytes, ModTime: now}); err != nil {
				return err
			}
			_, err := io.CopyN(tw, rc, f.Bytes)
			return err
		}()
		if err != nil {
			log.Printf("run bundle %s: %s: %v", execID, f.Path, err)
			return
		}
	}
	_ = tw.Close()
}
