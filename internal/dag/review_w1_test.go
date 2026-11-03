package dag

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A node whose task finished "done" without a checked final report fails, and its children are never sent - the
// worker's word alone is never fed to {{parent.reply}} (C-13).
func TestAnUncheckedDoneFailsTheNodeAndSendsNoChild(t *testing.T) {
	for _, tc := range []struct {
		name    string
		checked *bool
	}{
		{"checked false", func() *bool { b := false; return &b }()},
		{"checked not said", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			store := newTestStore(t, now)
			d, _ := store.Create("pipeline")
			parent, _ := store.AddNode(d.ID, "parent", "box-1", "do the thing")
			child, _ := store.AddNode(d.ID, "child", "box-2", "use {{parent.reply}}")
			if _, err := store.AddEdge(d.ID, parent.ID, child.ID); err != nil {
				t.Fatal(err)
			}

			submitter := &fakeSubmitter{}
			results := newFakeTaskResults()
			runner := newTestRunner(store, submitter, results, now)

			run, err := runner.Trigger(context.Background(), d.ID, TriggerManual)
			if err != nil {
				t.Fatal(err)
			}
			rns, _ := store.RunNodes(run.ID)
			var taskID string
			for _, rn := range rns {
				if rn.NodeID == parent.ID {
					taskID = rn.TaskID
				}
			}
			if taskID == "" {
				t.Fatal("parent was not dispatched")
			}

			results.setUnchecked(taskID, "done", "all filed", tc.checked)
			runner.advance(context.Background(), run)

			rns, _ = store.RunNodes(run.ID)
			for _, rn := range rns {
				switch rn.NodeID {
				case parent.ID:
					if rn.Status != NodeFailed || !strings.Contains(rn.FinalReply, "without a checked final report") {
						t.Errorf("parent = %s %q, want failed for want of a check", rn.Status, rn.FinalReply)
					}
				case child.ID:
					if rn.Status != NodeSkipped {
						t.Errorf("child = %s, want skipped", rn.Status)
					}
				}
			}
			if got := submitter.callsFor("box-2"); len(got) != 0 {
				t.Errorf("child was sent %q", got)
			}
			if final, _ := store.getRun(run.ID); final.Status != RunFailed {
				t.Errorf("run = %s, want failed", final.Status)
			}
		})
	}
}

// A node claimed and never given its task (the control plane stopped between the claim and the send) is failed
// once its lease is out, with a reason a person can act on; the run ends and the node can be deleted (C-02).
func TestAClaimLeftByAStoppedControlPlaneFailsAfterItsLease(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := newTestStore(t, now)
	d, _ := s.Create("d")
	n, err := s.AddNode(d.ID, "a", "desk1", "hello")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.StartRun(d.ID, TriggerManual, now)
	if err != nil {
		t.Fatal(err)
	}
	rns, _ := s.RunNodes(run.ID)
	if ok, _ := s.ClaimRunNode(rns[0].ID, now); !ok {
		t.Fatal("claim")
	}

	sub := &fakeSubmitter{}
	clock := now.Add(time.Minute)
	r := &Runner{Store: s, Submitter: sub, Tasks: newFakeTaskResults(), now: func() time.Time { return clock }}

	r.advance(context.Background(), run)
	if got, _ := s.getRun(run.ID); got.Status != RunRunning {
		t.Fatalf("within the lease the run is %s, want still running (a send may be in flight)", got.Status)
	}

	clock = now.Add(claimLease + time.Minute)
	r.advance(context.Background(), run)

	got, _ := s.getRun(run.ID)
	rns, _ = s.RunNodes(run.ID)
	if got.Status != RunFailed || rns[0].Status != NodeFailed || !strings.Contains(rns[0].FinalReply, "check the desktop") {
		t.Fatalf("run=%s node=%s %q, want both failed with a reason", got.Status, rns[0].Status, rns[0].FinalReply)
	}
	if len(sub.calls) != 0 {
		t.Errorf("the node was sent again: %d sends", len(sub.calls))
	}
	if err := s.DeleteNode(n.ID); err != nil {
		t.Errorf("DeleteNode after the run ended: %v", err)
	}
}

// A claim older than this Runner's own start can have no send in flight here: it is failed at once.
func TestAClaimFromBeforeThisProcessFailsAtOnce(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := newTestStore(t, now)
	d, _ := s.Create("d")
	s.AddNode(d.ID, "a", "desk1", "hello")
	run, _ := s.StartRun(d.ID, TriggerManual, now)
	rns, _ := s.RunNodes(run.ID)
	s.ClaimRunNode(rns[0].ID, now)

	r := &Runner{Store: s, Submitter: &fakeSubmitter{}, Tasks: newFakeTaskResults(),
		now: func() time.Time { return now.Add(time.Second) }}
	r.markStarted(now.Add(time.Millisecond))
	r.advance(context.Background(), run)

	if got, _ := s.getRun(run.ID); got.Status != RunFailed {
		t.Fatalf("run = %s, want failed", got.Status)
	}
}

// A person can cancel a run: nothing more is sent, and the run is closed.
func TestCancelEndsARun(t *testing.T) {
	now := time.Now()
	store := newTestStore(t, now)
	d, _ := store.Create("pipeline")
	a, _ := store.AddNode(d.ID, "a", "box-1", "first")
	b, _ := store.AddNode(d.ID, "b", "box-2", "second")
	store.AddEdge(d.ID, a.ID, b.ID)

	submitter := &fakeSubmitter{}
	results := newFakeTaskResults()
	runner := newTestRunner(store, submitter, results, now)
	run, _ := runner.Trigger(context.Background(), d.ID, TriggerManual)

	ok, err := runner.Cancel(run.ID)
	if err != nil || !ok {
		t.Fatalf("Cancel = %v, %v", ok, err)
	}
	if again, _ := runner.Cancel(run.ID); again {
		t.Error("a closed run was cancelled twice")
	}
	rns, _ := store.RunNodes(run.ID)
	for _, rn := range rns {
		if rn.Status == NodePending || rn.Status == NodeRunning {
			t.Errorf("node %s still %s", rn.NodeID, rn.Status)
		}
	}
	runner.advance(context.Background(), run)
	if got := submitter.callsFor("box-2"); len(got) != 0 {
		t.Errorf("a cancelled run sent %q", got)
	}
}
