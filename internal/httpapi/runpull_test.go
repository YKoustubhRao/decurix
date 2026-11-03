package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/memory"
	"github.com/YKoustubhRao/decurix/internal/runs"
	"github.com/YKoustubhRao/decurix/internal/schedule"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// fakePuller records the pulls and flushes asked of it, in order, into log.
type fakePuller struct {
	mu      sync.Mutex
	log     *[]string
	fails   int // the first fails pulls fail
	pulled  []runs.ExecutionRef
	flushed []runs.ExecutionRef
	stored  map[string]bool
}

func (f *fakePuller) Pull(_ context.Context, ref runs.ExecutionRef) (runs.Bundle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulled = append(f.pulled, ref)
	if f.fails > 0 {
		f.fails--
		return runs.Bundle{}, errors.New("the desktop is busy")
	}
	return runs.Bundle{ExecID: ref.ExecID}, nil
}

func (f *fakePuller) FlushDesktop(_ context.Context, worker string, refs []runs.ExecutionRef, _ time.Duration) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	*f.log = append(*f.log, "flush "+worker)
	f.flushed = append(f.flushed, refs...)
	return nil
}

func (f *fakePuller) Has(execID string) (bool, error) { return f.stored[execID], nil }

// queued drains what the pull queue holds now.
func queued(s *Server) []runs.ExecutionRef {
	var out []runs.ExecutionRef
	for {
		select {
		case ref := <-s.runPulls.queue():
			out = append(out, ref)
		default:
			return out
		}
	}
}

// A final callback enqueues one pull of the execution it ended; a question does not, and a callback for a task
// already closed (an interrupted turn reported again) enqueues none.
func TestAnOutcomeEnqueuesItsBundlePull(t *testing.T) {
	s := chatServer(t)
	var order []string
	f := &fakePuller{log: &order}
	s.Runs, s.RunBundles = f, f

	route, _ := s.Tasks.Commit(tasks.NewRoute(), tasks.Worker{ID: "box", Name: "box"}, "count the bills")
	post := func(body map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/querytext", strings.NewReader(string(raw)))
		req = req.WithContext(context.WithValue(req.Context(), callerKey{}, "box"))
		s.queryTextHandler(httptest.NewRecorder(), req)
	}
	post(map[string]any{"desktop_id": "box", "kind": "query", "content": "Which month?", "task_id": route.TaskID})
	if got := queued(s); len(got) != 0 {
		t.Fatalf("a question enqueued %v", got)
	}
	reply, _ := s.Tasks.Plan("box", "October")
	_, _, _ = s.Tasks.CommitReply(reply, tasks.Worker{ID: "box", Name: "box"}, "October", false, tasks.ReplyBy{Actor: tasks.LocalOwner})
	post(map[string]any{"desktop_id": "box", "kind": "final", "content": "12", "task_id": route.TaskID, "checked": true})
	got := queued(s)
	if len(got) != 1 || got[0].ExecID != route.ExecutionID || got[0].TaskID != route.TaskID || got[0].Worker != "box" {
		t.Fatalf("a final enqueued %v", got)
	}
	post(map[string]any{"desktop_id": "box", "kind": "error", "content": "it stopped", "task_id": route.TaskID, "interrupted": true})
	if got := queued(s); len(got) != 0 {
		t.Fatalf("an interrupted report of a closed task enqueued %v", got)
	}
}

// A full queue drops the pull and says so; the callback never waits on it.
func TestAFullPullQueueDoesNotBlock(t *testing.T) {
	s := chatServer(t)
	var order []string
	f := &fakePuller{log: &order}
	s.Runs = f
	done := make(chan struct{})
	go func() {
		for i := 0; i < runPullQueue+5; i++ {
			s.enqueueRunPull(runs.ExecutionRef{ExecID: "x", TaskID: "t", Worker: "box"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("enqueueing blocked on a full queue")
	}
	if n := len(queued(s)); n != runPullQueue {
		t.Fatalf("%d queued, want %d", n, runPullQueue)
	}
}

// The pull worker waits, then tries a failing pull again, up to its limit.
func TestThePullWorkerTriesAgain(t *testing.T) {
	runPullDelay, runPullBackoff = time.Millisecond, time.Millisecond
	t.Cleanup(func() { runPullDelay, runPullBackoff = 5*time.Second, 10*time.Second })
	s := chatServer(t)
	var order []string
	f := &fakePuller{log: &order, fails: 2}
	s.Runs = f
	s.pullWithRetries(context.Background(), runs.ExecutionRef{ExecID: "x1", TaskID: "t1", Worker: "box"})
	if len(f.pulled) != 3 {
		t.Fatalf("%d tries, want 3 (two failures, then the pull)", len(f.pulled))
	}
	f.fails = 10
	f.pulled = nil
	s.pullWithRetries(context.Background(), runs.ExecutionRef{ExecID: "x2", TaskID: "t2", Worker: "box"})
	if len(f.pulled) != runPullTries {
		t.Fatalf("%d tries of a pull that keeps failing, want %d", len(f.pulled), runPullTries)
	}
}

// Deleting a desktop pulls every run of it with no bundle yet, before the desktop goes.
func TestDeleteFlushesRunsBeforeTheDesktopGoes(t *testing.T) {
	s := chatServer(t)
	fakeAgentDesktop(t, s)
	var err error
	if s.Memory, err = memory.NewStore(testDB(t)); err != nil {
		t.Fatal(err)
	}
	if s.Schedules, err = schedule.NewStore(testDB(t)); err != nil {
		t.Fatal(err)
	}
	var order []string
	pulledAlready := tasks.NewRoute()
	f := &fakePuller{log: &order, stored: map[string]bool{}}
	s.Runs, s.RunBundles = f, f
	s.deleteDesktop = func(_ context.Context, id string) error {
		order = append(order, "delete "+id)
		return nil
	}
	w := tasks.Worker{ID: "box", Name: "box"}
	a, _ := s.Tasks.Commit(tasks.NewRoute(), w, "one")
	_, _ = s.Tasks.RecordOutcome("box", a.TaskID, tasks.Outcome{Kind: tasks.OutcomeDone, Reply: "ok"})
	b, _ := s.Tasks.Commit(pulledAlready, w, "two")
	_, _ = s.Tasks.RecordOutcome("box", b.TaskID, tasks.Outcome{Kind: tasks.OutcomeDone, Reply: "ok"})
	f.stored[b.ExecutionID] = true

	req := httptest.NewRequest(http.MethodPost, "/api/desktops/delete", strings.NewReader(url.Values{"id": {"box"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.deleteDesktopHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if len(order) != 2 || order[0] != "flush box" || order[1] != "delete box" {
		t.Fatalf("order = %v, want the flush first", order)
	}
	if len(f.flushed) != 1 || f.flushed[0].ExecID != a.ExecutionID {
		t.Fatalf("flushed %v, want only the run with no bundle", f.flushed)
	}
}
