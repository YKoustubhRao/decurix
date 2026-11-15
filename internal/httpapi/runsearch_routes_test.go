// runsearch_routes_test.go checks the doors to the run search and the training export: a desktop reaches only its
// own runs through its allowlisted callback, never a person's route, and the export writes one line per checked run.
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/runs"
	"github.com/YKoustubhRao/decurix/internal/runsearch"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// searchServer is a server with task records, a run search index and two workers' finished tasks (box-a, box-b),
// guarded as the primary listener with 172.30.0.5 as box-a's address.
func searchServer(t *testing.T) (*Server, http.Handler, map[string]string) {
	t.Helper()
	db := testDB(t)
	ts, err := tasks.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{DB: db, Tasks: ts}
	s.netsFor = staticNets("172.30.0.0/16", "172.30.0.1")
	s.callerAt = func(r *http.Request) (string, bool, error) {
		if strings.HasPrefix(r.RemoteAddr, "172.30.0.5:") {
			return "box-a", true, nil
		}
		return "", false, nil
	}
	ix, err := runsearch.New(s.DB, s.Tasks)
	if err != nil {
		t.Fatal(err)
	}
	s.RunSearch = ix
	ids := map[string]string{}
	for _, w := range []string{"box-a", "box-b"} {
		route, err := s.Tasks.Commit(tasks.NewRoute(), tasks.Worker{ID: w, Name: w}, "renew the parking permit", tasks.Origin{Kind: tasks.OriginPerson})
		if err != nil {
			t.Fatal(err)
		}
		checked := true
		if _, err := s.Tasks.RecordOutcome(w, route.TaskID, tasks.Outcome{Kind: tasks.OutcomeDone, Reply: "permit renewed", Checked: &checked}); err != nil {
			t.Fatal(err)
		}
		if err := ix.IndexTask(route.TaskID); err != nil {
			t.Fatal(err)
		}
		ids[w] = route.TaskID
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/runs/search", s.humansOnly(s.runSearchPersonHandler))
	mux.HandleFunc("/api/agent/runs", s.runSearchDesktopHandler)
	return s, s.GuardPrimary(mux), ids
}

type searchAnswer struct {
	Shape string          `json:"shape"`
	Hits  []runsearch.Hit `json:"hits"`
	Run   runsearch.Run   `json:"run"`
}

func decodeSearch(t *testing.T, rec *httptest.ResponseRecorder) searchAnswer {
	t.Helper()
	var a searchAnswer
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &a) != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	return a
}

// A desktop's search through GET /api/agent/runs is allowlisted (a missing entry gave 403 to every desktop once),
// answers its own runs only whatever it names, reads none of another's, cannot post, and never reaches the person's
// route.
func TestADesktopSearchesOnlyItsOwnRuns(t *testing.T) {
	_, h, ids := searchServer(t)
	const fromDesktop = "172.30.0.5:40000"
	host := map[string]string{"Host": "127.0.0.1:8080"}
	if !containerMayCall(http.MethodGet, "/api/agent/runs") || containerMayCall(http.MethodPost, "/api/agent/runs") {
		t.Fatal("the desktop route is not allowlisted read only")
	}

	got := decodeSearch(t, guardDo(h, http.MethodGet, "/api/agent/runs?q=parking+permit", fromDesktop, host))
	if got.Shape != "discover" || len(got.Hits) != 1 || got.Hits[0].TaskID != ids["box-a"] {
		t.Fatalf("own search: %+v", got)
	}
	if rec := guardDo(h, http.MethodGet, "/api/agent/runs?q=parking&desktop_id=box-b", fromDesktop, host); rec.Code != http.StatusForbidden {
		t.Fatalf("searching as another desktop: %d", rec.Code)
	}
	if rec := guardDo(h, http.MethodGet, "/api/agent/runs?task="+ids["box-b"], fromDesktop, host); rec.Code != http.StatusNotFound {
		t.Fatalf("reading another desktop's run: %d %s", rec.Code, rec.Body)
	}
	read := decodeSearch(t, guardDo(h, http.MethodGet, "/api/agent/runs?task="+ids["box-a"], fromDesktop, host))
	if read.Shape != "read" || read.Run.TaskID != ids["box-a"] || len(read.Run.Messages) != 2 {
		t.Fatalf("own read: %+v", read)
	}
	scroll := decodeSearch(t, guardDo(h, http.MethodGet, "/api/agent/runs?task="+ids["box-a"]+"&around=1&window=1", fromDesktop, host))
	if scroll.Shape != "scroll" || len(scroll.Run.Messages) != 2 {
		t.Fatalf("own scroll: %+v", scroll)
	}
	if rec := guardDo(h, http.MethodPost, "/api/agent/runs?q=parking", fromDesktop, host); rec.Code != http.StatusForbidden {
		t.Fatalf("a desktop posted: %d", rec.Code)
	}
	if rec := guardDo(h, http.MethodGet, "/api/runs/search?q=parking", fromDesktop, host); rec.Code != http.StatusForbidden {
		t.Fatalf("a desktop reached the person's search: %d", rec.Code)
	}
}

// A person searches every worker's runs, or one worker's; a bad bound or no words is a 400, never an echo.
func TestAPersonSearchesEveryRun(t *testing.T) {
	_, h, ids := searchServer(t)
	host := map[string]string{"Host": "127.0.0.1:8080"}
	all := decodeSearch(t, guardDo(h, http.MethodGet, "/api/runs/search?q=permit", fromHost, host))
	if len(all.Hits) != 2 {
		t.Fatalf("hits: %+v", all.Hits)
	}
	one := decodeSearch(t, guardDo(h, http.MethodGet, "/api/runs/search?q=permit&worker=box-b", fromHost, host))
	if len(one.Hits) != 1 || one.Hits[0].TaskID != ids["box-b"] {
		t.Fatalf("one worker: %+v", one.Hits)
	}
	if rec := guardDo(h, http.MethodGet, "/api/runs/search?q=permit&after=yesterday-ish", fromHost, host); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad bound: %d", rec.Code)
	}
	if rec := guardDo(h, http.MethodGet, "/api/runs/search?q=", fromHost, host); rec.Code != http.StatusBadRequest {
		t.Fatalf("no words: %d", rec.Code)
	}
	if rec := guardDo(h, http.MethodGet, "/api/agent/runs?q=permit", fromHost, host); rec.Code != http.StatusBadRequest {
		t.Fatalf("the desktop route from the host without a desktop: %d", rec.Code)
	}
}

// The export writes one JSON line per run whose verdict is a checked done, and says how many it wrote.
func TestTheTrainingExportWritesCheckedRunsOnly(t *testing.T) {
	s, _, ids := searchServer(t)
	execOf := map[string]string{}
	for w, task := range ids {
		d, err := s.Tasks.Get(task)
		if err != nil {
			t.Fatal(err)
		}
		execOf[d.Executions[0].ID] = w
	}
	s.buildRun = func(_ context.Context, execID string) (runs.Trajectory, error) {
		basis := `["model"]`
		if execOf[execID] == "box-a" {
			basis = `["code"]`
		}
		return runs.Trajectory{Schema: runs.Schema, RunID: execID, Request: runs.Request{Text: "renew the parking permit"},
			Verdict: json.RawMessage(`{"verdict":"done","basis":` + basis + `}`), Labels: runs.Labels{Run: "done"}}, nil
	}
	rec := httptest.NewRecorder()
	s.trainingExportHandler(rec, httptest.NewRequest(http.MethodGet, "/api/runs/training-export?since=1d", nil))
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if rec.Code != http.StatusOK || len(lines) != 1 || !strings.Contains(lines[0], `"schema":"decurix.sft/v1"`) ||
		!strings.Contains(lines[0], "DONE:\\npermit renewed") {
		t.Fatalf("%d %q", rec.Code, rec.Body)
	}
	if got := rec.Result().Trailer.Get("X-Decurix-Exported"); got != "1" {
		t.Fatalf("exported trailer %q", got)
	}
	bad := httptest.NewRecorder()
	s.trainingExportHandler(bad, httptest.NewRequest(http.MethodGet, "/api/runs/training-export?format=prose", nil))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("unknown format: %d", bad.Code)
	}
}
