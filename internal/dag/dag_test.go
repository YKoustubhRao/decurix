package dag

import (
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

func newTestStore(t *testing.T, now time.Time) *Store {
	t.Helper()

	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}

	store.now = func() time.Time { return now }

	return store
}

func TestFirstNodeBecomesHeadAutomatically(t *testing.T) {
	store := newTestStore(t, time.Now())

	d, err := store.Create("pipeline")
	if err != nil {
		t.Fatal(err)
	}

	if d.HeadNodeID != "" {
		t.Fatalf("a fresh DAG should have no head, got %q", d.HeadNodeID)
	}

	first, err := store.AddNode(d.ID, "fetch", "box-1", "fetch the data")
	if err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(d.ID)
	if err != nil {
		t.Fatal(err)
	}

	if got.HeadNodeID != first.ID {
		t.Fatalf("HeadNodeID = %q, want the first node %q", got.HeadNodeID, first.ID)
	}

	second, err := store.AddNode(d.ID, "process", "box-1", "process it")
	if err != nil {
		t.Fatal(err)
	}

	got, err = store.Get(d.ID)
	if err != nil {
		t.Fatal(err)
	}

	if got.HeadNodeID != first.ID {
		t.Fatalf("adding a second node changed the head to %q, want it to stay %q", got.HeadNodeID, first.ID)
	}

	_ = second
}

func TestAddNodeRejectsDuplicateNameInSameDag(t *testing.T) {
	store := newTestStore(t, time.Now())
	d, _ := store.Create("pipeline")

	if _, err := store.AddNode(d.ID, "fetch", "box-1", ""); err != nil {
		t.Fatal(err)
	}

	if _, err := store.AddNode(d.ID, "fetch", "box-2", ""); err == nil {
		t.Fatal("expected a duplicate node name to be rejected")
	}
}

func TestAddNodeRejectsInvalidNameCharset(t *testing.T) {
	store := newTestStore(t, time.Now())
	d, _ := store.Create("pipeline")

	if _, err := store.AddNode(d.ID, "fetch data!", "box-1", ""); err == nil {
		t.Fatal("expected an invalid node name to be rejected")
	}
}

func TestSetHeadRejectsNodeWithIncomingEdges(t *testing.T) {
	store := newTestStore(t, time.Now())
	d, _ := store.Create("pipeline")

	a, _ := store.AddNode(d.ID, "a", "box-1", "")
	b, _ := store.AddNode(d.ID, "b", "box-1", "")

	if _, err := store.AddEdge(d.ID, a.ID, b.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := store.SetHead(d.ID, b.ID); err == nil {
		t.Fatal("expected SetHead to reject a node with an incoming edge")
	}
}

func TestSetHeadPromotesNewHeadOldHeadBecomesOrdinary(t *testing.T) {
	store := newTestStore(t, time.Now())
	d, _ := store.Create("pipeline")

	a, _ := store.AddNode(d.ID, "a", "box-1", "")
	b, _ := store.AddNode(d.ID, "b", "box-1", "")

	got, err := store.SetHead(d.ID, b.ID)
	if err != nil {
		t.Fatal(err)
	}

	if got.HeadNodeID != b.ID {
		t.Fatalf("HeadNodeID = %q, want %q", got.HeadNodeID, b.ID)
	}

	_ = a
}

func TestAddEdgeRejectsSelfLoop(t *testing.T) {
	store := newTestStore(t, time.Now())
	d, _ := store.Create("pipeline")
	a, _ := store.AddNode(d.ID, "a", "box-1", "")

	if _, err := store.AddEdge(d.ID, a.ID, a.ID); err == nil {
		t.Fatal("expected a self-loop to be rejected")
	}
}

func TestAddEdgeRejectsIntoHead(t *testing.T) {
	store := newTestStore(t, time.Now())
	d, _ := store.Create("pipeline")

	a, _ := store.AddNode(d.ID, "a", "box-1", "") // becomes head
	b, _ := store.AddNode(d.ID, "b", "box-1", "")

	if _, err := store.AddEdge(d.ID, b.ID, a.ID); err == nil {
		t.Fatal("expected an edge into the head to be rejected")
	}
}

func TestAddEdgeRejectsDuplicatePair(t *testing.T) {
	store := newTestStore(t, time.Now())
	d, _ := store.Create("pipeline")

	a, _ := store.AddNode(d.ID, "a", "box-1", "")
	b, _ := store.AddNode(d.ID, "b", "box-1", "")

	if _, err := store.AddEdge(d.ID, a.ID, b.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := store.AddEdge(d.ID, a.ID, b.ID); err == nil {
		t.Fatal("expected a duplicate edge to be rejected")
	}
}

func TestAddEdgeRejectsCycle(t *testing.T) {
	store := newTestStore(t, time.Now())
	d, _ := store.Create("pipeline")

	a, _ := store.AddNode(d.ID, "a", "box-1", "")
	b, _ := store.AddNode(d.ID, "b", "box-1", "")

	if _, err := store.AddEdge(d.ID, a.ID, b.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := store.AddEdge(d.ID, b.ID, a.ID); err == nil {
		t.Fatal("expected a direct cycle to be rejected")
	}

	c, _ := store.AddNode(d.ID, "c", "box-1", "")

	if _, err := store.AddEdge(d.ID, b.ID, c.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := store.AddEdge(d.ID, c.ID, a.ID); err == nil {
		t.Fatal("expected a 3-hop cycle to be rejected")
	}
}

func TestAddEdgeAllowsDiamond(t *testing.T) {
	store := newTestStore(t, time.Now())
	d, _ := store.Create("pipeline")

	a, _ := store.AddNode(d.ID, "a", "box-1", "")
	b, _ := store.AddNode(d.ID, "b", "box-1", "")
	c, _ := store.AddNode(d.ID, "c", "box-1", "")
	e, _ := store.AddNode(d.ID, "e", "box-1", "")

	for _, pair := range [][2]string{{a.ID, b.ID}, {a.ID, c.ID}, {b.ID, e.ID}, {c.ID, e.ID}} {
		if _, err := store.AddEdge(d.ID, pair[0], pair[1]); err != nil {
			t.Fatalf("AddEdge(%s, %s): %v", pair[0], pair[1], err)
		}
	}
}

func TestDeleteNodeCascadesEdges(t *testing.T) {
	store := newTestStore(t, time.Now())
	d, _ := store.Create("pipeline")

	a, _ := store.AddNode(d.ID, "a", "box-1", "")
	b, _ := store.AddNode(d.ID, "b", "box-1", "")

	if _, err := store.AddEdge(d.ID, a.ID, b.ID); err != nil {
		t.Fatal(err)
	}

	if err := store.DeleteNode(b.ID); err != nil {
		t.Fatal(err)
	}

	edges, err := store.Edges(d.ID)
	if err != nil {
		t.Fatal(err)
	}

	if len(edges) != 0 {
		t.Fatalf("edges = %v, want none once one end is deleted", edges)
	}
}

func TestDeleteNodeClearsHeadWhenHeadDeleted(t *testing.T) {
	store := newTestStore(t, time.Now())
	d, _ := store.Create("pipeline")

	a, _ := store.AddNode(d.ID, "a", "box-1", "")

	if err := store.DeleteNode(a.ID); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(d.ID)
	if err != nil {
		t.Fatal(err)
	}

	if got.HeadNodeID != "" {
		t.Fatalf("HeadNodeID = %q, want cleared", got.HeadNodeID)
	}
}

func TestDeleteNodeRefusedWhileALiveRunDependsOnIt(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	store := newTestStore(t, now)
	d, _ := store.Create("pipeline")

	a, _ := store.AddNode(d.ID, "a", "box-1", "")

	if _, err := store.StartRun(d.ID, TriggerManual, now); err != nil {
		t.Fatal(err)
	}

	if err := store.DeleteNode(a.ID); err == nil {
		t.Fatal("expected deletion to be refused while a run depends on this node")
	}
}

func TestDeleteEdgeRefusedWhileALiveRunIsWaitingOnIt(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	store := newTestStore(t, now)
	d, _ := store.Create("pipeline")

	a, _ := store.AddNode(d.ID, "a", "box-a", "")
	b, _ := store.AddNode(d.ID, "b", "box-b", "")

	edge, err := store.AddEdge(d.ID, a.ID, b.ID)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.StartRun(d.ID, TriggerManual, now); err != nil {
		t.Fatal(err)
	}

	// b is still pending, waiting on a - removing a->b out from under it
	// would let b dispatch without ever having waited for a.
	if err := store.DeleteEdge(edge.ID); err == nil {
		t.Fatal("expected deletion to be refused while a run is waiting on this edge's node")
	}
}

func TestSetCronClearsOnEmptyString(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 30, 0, 0, time.UTC)
	store := newTestStore(t, now)
	d, _ := store.Create("pipeline")

	if _, err := store.SetCron(d.ID, "*/10 * * * *"); err != nil {
		t.Fatal(err)
	}

	got, err := store.SetCron(d.ID, "")
	if err != nil {
		t.Fatal(err)
	}

	if got.CronExpr != "" || !got.NextRunAt.IsZero() {
		t.Fatalf("SetCron(\"\") = %+v, want cleared", got)
	}
}

func TestSetCronSchedulesNextOccurrenceNotNow(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 30, 0, 0, time.UTC)
	store := newTestStore(t, now)
	d, _ := store.Create("pipeline")

	got, err := store.SetCron(d.ID, "*/10 * * * *")
	if err != nil {
		t.Fatal(err)
	}

	want := time.Date(2026, 9, 15, 12, 40, 0, 0, time.UTC)
	if !got.NextRunAt.Equal(want) {
		t.Fatalf("NextRunAt = %v, want %v", got.NextRunAt, want)
	}
}

func TestSetCronRejectsBadCron(t *testing.T) {
	store := newTestStore(t, time.Now())
	d, _ := store.Create("pipeline")

	if _, err := store.SetCron(d.ID, "not a cron"); err == nil {
		t.Fatal("expected an invalid cron expression to be rejected")
	}
}

func TestDueCronReturnsOnlyDueOnes(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 30, 0, 0, time.UTC)
	store := newTestStore(t, now)

	due, _ := store.Create("due")
	if _, err := store.SetCron(due.ID, "*/10 * * * *"); err != nil {
		t.Fatal(err)
	}

	notDue, _ := store.Create("not due")
	if _, err := store.SetCron(notDue.ID, "0 0 1 1 *"); err != nil {
		t.Fatal(err)
	}

	list, err := store.DueCron(now.Add(11 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	if len(list) != 1 || list[0].ID != due.ID {
		t.Fatalf("DueCron = %v, want only %q", list, due.ID)
	}
}

func TestDeleteDagCascadesNodesEdgesRunsRunNodes(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	store := newTestStore(t, now)
	d, _ := store.Create("pipeline")

	a, _ := store.AddNode(d.ID, "a", "box-1", "")
	b, _ := store.AddNode(d.ID, "b", "box-1", "")

	if _, err := store.AddEdge(d.ID, a.ID, b.ID); err != nil {
		t.Fatal(err)
	}

	run, err := store.StartRun(d.ID, TriggerManual, now)
	if err != nil {
		t.Fatal(err)
	}

	store.Delete(d.ID)

	if _, err := store.Get(d.ID); err != ErrNotFound {
		t.Fatalf("Get after delete = %v, want ErrNotFound", err)
	}

	runNodes, err := store.RunNodes(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	if len(runNodes) != 0 {
		t.Fatalf("run nodes survived deleting the DAG: %v", runNodes)
	}
}

func TestStartRunRejectsHeadlessDag(t *testing.T) {
	now := time.Now()
	store := newTestStore(t, now)
	d, _ := store.Create("pipeline")

	if _, err := store.StartRun(d.ID, TriggerManual, now); err == nil {
		t.Fatal("expected StartRun to reject a DAG with no head")
	}
}

func TestStartRunMaterializesOnlyNodesReachableFromHead(t *testing.T) {
	now := time.Now()
	store := newTestStore(t, now)
	d, _ := store.Create("pipeline")

	head, _ := store.AddNode(d.ID, "head", "box-1", "")
	reachable, _ := store.AddNode(d.ID, "reachable", "box-1", "")
	orphan, _ := store.AddNode(d.ID, "orphan", "box-1", "")

	if _, err := store.AddEdge(d.ID, head.ID, reachable.ID); err != nil {
		t.Fatal(err)
	}

	run, err := store.StartRun(d.ID, TriggerManual, now)
	if err != nil {
		t.Fatal(err)
	}

	runNodes, err := store.RunNodes(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	if len(runNodes) != 2 {
		t.Fatalf("run nodes = %v, want exactly the head and reachable node", runNodes)
	}

	for _, rn := range runNodes {
		if rn.NodeID == orphan.ID {
			t.Fatalf("orphan node got a run-node: %+v", rn)
		}
	}
}

func TestMarkRunNodeDispatchedIsCompareAndSwap(t *testing.T) {
	now := time.Now()
	store := newTestStore(t, now)
	d, _ := store.Create("pipeline")
	store.AddNode(d.ID, "head", "box-1", "")

	run, err := store.StartRun(d.ID, TriggerManual, now)
	if err != nil {
		t.Fatal(err)
	}

	runNodes, err := store.RunNodes(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	rn := runNodes[0]

	applied, err := store.MarkRunNodeDispatched(rn.ID, "t1", now)
	if err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("first dispatch should apply")
	}

	applied, err = store.MarkRunNodeDispatched(rn.ID, "t2", now)
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("a second dispatch from the same (now non-pending) row should not apply")
	}
}
