package httpapi

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/blobstore"
	"github.com/YKoustubhRao/decurix/internal/runs"
)

// The run record routes serve what runs.Build makes, and the bundle as one tar with trajectory.json beside the
// files; an unknown run is 404. (The gated listener never has them: TestNoPrimaryOnlyRouteIsOnTheGatedListener.)
func TestTheRunRecordIsServed(t *testing.T) {
	s := chatServer(t)
	blobs, err := blobstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if s.RunStore, err = runs.NewStore(testDB(t), blobs); err != nil {
		t.Fatal(err)
	}
	ledger := []byte(`{"records":[]}`)
	sha, _ := blobs.PutBytes(ledger)
	if err := s.RunStore.SaveBundle(runs.Bundle{ExecID: "x1", TaskID: "t1", Worker: "box", PulledAt: time.Now(),
		Files: []runs.File{{Path: ".decurix/computer/evidence/t1.records.json", SHA256: sha, Bytes: int64(len(ledger))},
			{Path: ".decurix/runs/t1/run.json", SHA256: blobstore.SHA256([]byte("gone")), Bytes: 4}}}); err != nil {
		t.Fatal(err)
	}
	s.buildRun = func(_ context.Context, execID string) (runs.Trajectory, error) {
		if execID != "x1" {
			return runs.Trajectory{}, runs.ErrUnknownExecution
		}
		return runs.Trajectory{Schema: "decurix.trajectory/v1", RunID: "x1", TaskID: "t1"}, nil
	}
	get := func(p string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.runsHandler(rec, httptest.NewRequest(http.MethodGet, p, nil))
		return rec
	}

	rec := get("/api/runs/x1/trajectory")
	var doc map[string]any
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &doc) != nil || doc["run_id"] != "x1" {
		t.Fatalf("trajectory: %d %s", rec.Code, rec.Body)
	}
	if rec := get("/api/runs/x9/trajectory"); rec.Code != http.StatusNotFound {
		t.Fatalf("an unknown run: %d", rec.Code)
	}
	if rec := get("/api/runs/x1/elsewhere"); rec.Code != http.StatusNotFound {
		t.Fatalf("an unknown part: %d", rec.Code)
	}

	rec = get("/api/runs/x1/bundle")
	if rec.Code != 200 {
		t.Fatalf("bundle: %d %s", rec.Code, rec.Body)
	}
	got := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(rec.Body.Bytes()))
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		got[h.Name], _ = io.ReadAll(tr)
	}
	if string(got["home/.decurix/computer/evidence/t1.records.json"]) != string(ledger) {
		t.Fatalf("the ledger is not in the bundle: %v", keysOf(got))
	}
	var traj runs.Trajectory
	if err := json.Unmarshal(got["trajectory.json"], &traj); err != nil || traj.RunID != "x1" {
		t.Fatalf("trajectory.json = %s", got["trajectory.json"])
	}
	if len(traj.Integrity.Missing) != 1 || len(got) != 2 {
		t.Fatalf("a file whose copy is gone must be named missing, not sent: %v, %v", traj.Integrity.Missing, keysOf(got))
	}
}

func keysOf(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
