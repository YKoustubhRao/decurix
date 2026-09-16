package dag

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/YKoustubhRao/flowera/internal/tasks"
)

// fakeSubmitter is a Submitter a test can script and inspect.
type fakeSubmitter struct {
	mu     sync.Mutex
	calls  []struct{ desktopID, content string }
	nextID int
	errFor map[string]error // desktopID -> error to return instead of dispatching
}

func (f *fakeSubmitter) SubmitTracked(_ context.Context, desktopID, content string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, struct{ desktopID, content string }{desktopID, content})

	if err := f.errFor[desktopID]; err != nil {
		return "", err
	}

	f.nextID++

	return fmt.Sprintf("t%d", f.nextID), nil
}

func (f *fakeSubmitter) callsFor(desktopID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []string
	for _, c := range f.calls {
		if c.desktopID == desktopID {
			out = append(out, c.content)
		}
	}

	return out
}

// fakeTaskResults is a TaskResults a test can script: set a task's result
// once its node should be treated as finished.
type fakeTaskResults struct {
	mu      sync.Mutex
	results map[string]struct{ status, reply string }
}

func newFakeTaskResults() *fakeTaskResults {
	return &fakeTaskResults{results: map[string]struct{ status, reply string }{}}
}

func (f *fakeTaskResults) TaskResult(taskID string) (string, string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	r, ok := f.results[taskID]
	if !ok {
		return "", "", false
	}

	return r.status, r.reply, true
}

func (f *fakeTaskResults) set(taskID, status, reply string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results[taskID] = struct{ status, reply string }{status, reply}
}

func newTestRunner(store *Store, submitter Submitter, results TaskResults, now time.Time) *Runner {
	return &Runner{Store: store, Submitter: submitter, Tasks: results, now: func() time.Time { return now }}
}

func TestTriggerDispatchesHeadImmediately(t *testing.T) {
	now := time.Now()
	store := newTestStore(t, now)
	d, _ := store.Create("pipeline")
	store.AddNode(d.ID, "head", "box-1", "do the thing")

	submitter := &fakeSubmitter{}
	runner := newTestRunner(store, submitter, newFakeTaskResults(), now)

	if _, err := runner.Trigger(context.Background(), d.ID, TriggerManual); err != nil {
		t.Fatal(err)
	}

	calls := submitter.callsFor("box-1")
	if len(calls) != 1 || calls[0] != "do the thing" {
		t.Fatalf("calls to box-1 = %v, want exactly one dispatch of the head's content", calls)
	}
}

func TestChainDispatchesSecondNodeOnlyAfterFirstSucceeds(t *testing.T) {
	now := time.Now()
	store := newTestStore(t, now)
	d, _ := store.Create("pipeline")

	a, _ := store.AddNode(d.ID, "a", "box-a", "step a")
	b, _ := store.AddNode(d.ID, "b", "box-b", "step b")

	if _, err := store.AddEdge(d.ID, a.ID, b.ID); err != nil {
		t.Fatal(err)
	}

	submitter := &fakeSubmitter{}
	results := newFakeTaskResults()
	runner := newTestRunner(store, submitter, results, now)

	run, err := runner.Trigger(context.Background(), d.ID, TriggerManual)
	if err != nil {
		t.Fatal(err)
	}

	if calls := submitter.callsFor("box-b"); len(calls) != 0 {
		t.Fatalf("box-b dispatched before box-a finished: %v", calls)
	}

	runNodes, _ := store.RunNodes(run.ID)
	var aTaskID string
	for _, rn := range runNodes {
		if rn.NodeID == a.ID {
			aTaskID = rn.TaskID
		}
	}

	results.set(aTaskID, tasks.StatusDone, "a is done")
	runner.advance(context.Background(), run)

	if calls := submitter.callsFor("box-b"); len(calls) != 1 {
		t.Fatalf("box-b calls = %v, want exactly one dispatch now that a succeeded", calls)
	}
}

func TestDiamondSkipPropagationOnFailure(t *testing.T) {
	now := time.Now()
	store := newTestStore(t, now)
	d, _ := store.Create("pipeline")

	a, _ := store.AddNode(d.ID, "a", "box-a", "")
	b, _ := store.AddNode(d.ID, "b", "box-b", "")
	c, _ := store.AddNode(d.ID, "c", "box-c", "")
	e, _ := store.AddNode(d.ID, "e", "box-e", "")

	for _, pair := range [][2]string{{a.ID, b.ID}, {a.ID, c.ID}, {b.ID, e.ID}, {c.ID, e.ID}} {
		if _, err := store.AddEdge(d.ID, pair[0], pair[1]); err != nil {
			t.Fatal(err)
		}
	}

	submitter := &fakeSubmitter{}
	results := newFakeTaskResults()
	runner := newTestRunner(store, submitter, results, now)

	run, err := runner.Trigger(context.Background(), d.ID, TriggerManual)
	if err != nil {
		t.Fatal(err)
	}

	taskIDFor := func(nodeID string) string {
		runNodes, _ := store.RunNodes(run.ID)
		for _, rn := range runNodes {
			if rn.NodeID == nodeID {
				return rn.TaskID
			}
		}
		return ""
	}

	results.set(taskIDFor(a.ID), tasks.StatusDone, "a done")
	runner.advance(context.Background(), run)

	// b fails, c succeeds.
	results.set(taskIDFor(b.ID), tasks.StatusError, "b failed")
	results.set(taskIDFor(c.ID), tasks.StatusDone, "c done")
	runner.advance(context.Background(), run)
	runner.advance(context.Background(), run)

	runNodes, _ := store.RunNodes(run.ID)
	statusOf := make(map[string]string, len(runNodes))
	for _, rn := range runNodes {
		statusOf[rn.NodeID] = rn.Status
	}

	if statusOf[b.ID] != NodeFailed {
		t.Fatalf("b status = %q, want failed", statusOf[b.ID])
	}

	if statusOf[e.ID] != NodeSkipped {
		t.Fatalf("e status = %q, want skipped (b, one of its required parents, failed)", statusOf[e.ID])
	}

	if statusOf[c.ID] != NodeSucceeded {
		t.Fatalf("c status = %q, want succeeded - b's failure must not affect its sibling branch", statusOf[c.ID])
	}

	final, err := store.getRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	if final.Status != RunFailed {
		t.Fatalf("run status = %q, want failed", final.Status)
	}
}

func TestDispatchFailurePropagatesSkip(t *testing.T) {
	now := time.Now()
	store := newTestStore(t, now)
	d, _ := store.Create("pipeline")

	a, _ := store.AddNode(d.ID, "a", "box-a", "")
	b, _ := store.AddNode(d.ID, "b", "box-b", "")

	if _, err := store.AddEdge(d.ID, a.ID, b.ID); err != nil {
		t.Fatal(err)
	}

	submitter := &fakeSubmitter{errFor: map[string]error{"box-a": errors.New("desktop not found")}}
	runner := newTestRunner(store, submitter, newFakeTaskResults(), now)

	run, err := runner.Trigger(context.Background(), d.ID, TriggerManual)
	if err != nil {
		t.Fatal(err)
	}

	runNodes, _ := store.RunNodes(run.ID)
	statusOf := make(map[string]string, len(runNodes))
	for _, rn := range runNodes {
		statusOf[rn.NodeID] = rn.Status
	}

	if statusOf[a.ID] != NodeFailed {
		t.Fatalf("a status = %q, want failed", statusOf[a.ID])
	}

	if statusOf[b.ID] != NodeSkipped {
		t.Fatalf("b status = %q, want skipped", statusOf[b.ID])
	}
}

func TestOutputPlaceholderByName(t *testing.T) {
	now := time.Now()
	store := newTestStore(t, now)
	d, _ := store.Create("pipeline")

	fetch, _ := store.AddNode(d.ID, "fetch", "box-a", "")
	use, _ := store.AddNode(d.ID, "use", "box-b", "Use {{nodes.fetch.reply}}")

	if _, err := store.AddEdge(d.ID, fetch.ID, use.ID); err != nil {
		t.Fatal(err)
	}

	submitter := &fakeSubmitter{}
	results := newFakeTaskResults()
	runner := newTestRunner(store, submitter, results, now)

	run, err := runner.Trigger(context.Background(), d.ID, TriggerManual)
	if err != nil {
		t.Fatal(err)
	}

	runNodes, _ := store.RunNodes(run.ID)
	var fetchTaskID string
	for _, rn := range runNodes {
		if rn.NodeID == fetch.ID {
			fetchTaskID = rn.TaskID
		}
	}

	results.set(fetchTaskID, tasks.StatusDone, "42")
	runner.advance(context.Background(), run)

	calls := submitter.callsFor("box-b")
	if len(calls) != 1 || calls[0] != "Use 42" {
		t.Fatalf("box-b calls = %v, want [%q]", calls, "Use 42")
	}
}

func TestOutputPlaceholderParentSugarSingleParent(t *testing.T) {
	now := time.Now()
	store := newTestStore(t, now)
	d, _ := store.Create("pipeline")

	a, _ := store.AddNode(d.ID, "a", "box-a", "")
	b, _ := store.AddNode(d.ID, "b", "box-b", "got: {{parent.reply}}")

	if _, err := store.AddEdge(d.ID, a.ID, b.ID); err != nil {
		t.Fatal(err)
	}

	submitter := &fakeSubmitter{}
	results := newFakeTaskResults()
	runner := newTestRunner(store, submitter, results, now)

	run, err := runner.Trigger(context.Background(), d.ID, TriggerManual)
	if err != nil {
		t.Fatal(err)
	}

	runNodes, _ := store.RunNodes(run.ID)
	var aTaskID string
	for _, rn := range runNodes {
		if rn.NodeID == a.ID {
			aTaskID = rn.TaskID
		}
	}

	results.set(aTaskID, tasks.StatusDone, "hi")
	runner.advance(context.Background(), run)

	calls := submitter.callsFor("box-b")
	if len(calls) != 1 || calls[0] != "got: hi" {
		t.Fatalf("box-b calls = %v, want [%q]", calls, "got: hi")
	}
}

func TestOutputPlaceholderParentSugarLeftLiteralWithTwoParents(t *testing.T) {
	now := time.Now()
	store := newTestStore(t, now)
	d, _ := store.Create("pipeline")

	a, _ := store.AddNode(d.ID, "a", "box-a", "")
	b, _ := store.AddNode(d.ID, "b", "box-b", "")
	c, _ := store.AddNode(d.ID, "c", "box-c", "got: {{parent.reply}}")

	if _, err := store.AddEdge(d.ID, a.ID, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddEdge(d.ID, b.ID, c.ID); err != nil {
		t.Fatal(err)
	}

	submitter := &fakeSubmitter{}
	results := newFakeTaskResults()
	runner := newTestRunner(store, submitter, results, now)

	run, err := runner.Trigger(context.Background(), d.ID, TriggerManual)
	if err != nil {
		t.Fatal(err)
	}

	runNodes, _ := store.RunNodes(run.ID)
	taskIDFor := func(nodeID string) string {
		for _, rn := range runNodes {
			if rn.NodeID == nodeID {
				return rn.TaskID
			}
		}
		return ""
	}

	results.set(taskIDFor(a.ID), tasks.StatusDone, "from a")
	results.set(taskIDFor(b.ID), tasks.StatusDone, "from b")
	runner.advance(context.Background(), run)

	calls := submitter.callsFor("box-c")
	if len(calls) != 1 || calls[0] != "got: {{parent.reply}}" {
		t.Fatalf("box-c calls = %v, want the placeholder left literal with two parents", calls)
	}
}

func TestOutputPlaceholderLeftLiteralWhenNameUnknown(t *testing.T) {
	now := time.Now()
	store := newTestStore(t, now)
	d, _ := store.Create("pipeline")

	store.AddNode(d.ID, "head", "box-a", "see {{nodes.nope.reply}}")

	submitter := &fakeSubmitter{}
	runner := newTestRunner(store, submitter, newFakeTaskResults(), now)

	if _, err := runner.Trigger(context.Background(), d.ID, TriggerManual); err != nil {
		t.Fatal(err)
	}

	calls := submitter.callsFor("box-a")
	if len(calls) != 1 || calls[0] != "see {{nodes.nope.reply}}" {
		t.Fatalf("calls = %v, want the placeholder left literal", calls)
	}
}

func TestTickFiresDueCronDag(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 30, 0, 0, time.UTC)
	store := newTestStore(t, now)
	d, _ := store.Create("pipeline")
	store.AddNode(d.ID, "head", "box-a", "do it")

	if _, err := store.SetCron(d.ID, "*/10 * * * *"); err != nil {
		t.Fatal(err)
	}

	submitter := &fakeSubmitter{}
	firedAt := time.Date(2026, 9, 15, 12, 40, 0, 0, time.UTC)
	runner := newTestRunner(store, submitter, newFakeTaskResults(), firedAt)

	runner.tick(context.Background())

	calls := submitter.callsFor("box-a")
	if len(calls) != 1 || calls[0] != "do it" {
		t.Fatalf("calls = %v, want exactly one dispatch with the node's own content (no TIME field)", calls)
	}
}

func TestTickAdvancesCronEvenWhenTriggerFails(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 30, 0, 0, time.UTC)
	store := newTestStore(t, now)

	// A headless DAG can never StartRun, so its cron firing will fail - the
	// cron must still advance rather than retry the same stale occurrence
	// forever.
	d, _ := store.Create("headless")
	if _, err := store.SetCron(d.ID, "*/10 * * * *"); err != nil {
		t.Fatal(err)
	}

	firedAt := time.Date(2026, 9, 15, 12, 40, 0, 0, time.UTC)
	runner := newTestRunner(store, &fakeSubmitter{}, newFakeTaskResults(), firedAt)

	runner.tick(context.Background())

	got, err := store.Get(d.ID)
	if err != nil {
		t.Fatal(err)
	}

	if !got.LastRunAt.Equal(firedAt) {
		t.Fatalf("LastRunAt = %v, want %v (a failed trigger still consumes this run)", got.LastRunAt, firedAt)
	}

	if !got.NextRunAt.After(firedAt) {
		t.Fatalf("NextRunAt = %v, want something after %v", got.NextRunAt, firedAt)
	}
}

func TestRunSucceedsWhenEveryReachableNodeSucceeds(t *testing.T) {
	now := time.Now()
	store := newTestStore(t, now)
	d, _ := store.Create("pipeline")

	a, _ := store.AddNode(d.ID, "a", "box-a", "")
	b, _ := store.AddNode(d.ID, "b", "box-b", "")

	if _, err := store.AddEdge(d.ID, a.ID, b.ID); err != nil {
		t.Fatal(err)
	}

	submitter := &fakeSubmitter{}
	results := newFakeTaskResults()
	runner := newTestRunner(store, submitter, results, now)

	run, err := runner.Trigger(context.Background(), d.ID, TriggerManual)
	if err != nil {
		t.Fatal(err)
	}

	taskIDFor := func(nodeID string) string {
		runNodes, _ := store.RunNodes(run.ID)
		for _, rn := range runNodes {
			if rn.NodeID == nodeID {
				return rn.TaskID
			}
		}
		return ""
	}

	results.set(taskIDFor(a.ID), tasks.StatusDone, "a done")
	runner.advance(context.Background(), run)

	results.set(taskIDFor(b.ID), tasks.StatusDone, "b done")
	runner.advance(context.Background(), run)

	final, err := store.getRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	if final.Status != RunSucceeded {
		t.Fatalf("run status = %q, want succeeded", final.Status)
	}
}
