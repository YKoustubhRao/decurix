package tasks

import (
	"testing"
	"time"
)

// An outcome lands only on the execution it names (review 2026-11-03, C-01): a resent "done" for a task that ended
// must never close the task that is live now.
func TestAStaleOutcomeNeverClosesTheLiveTask(t *testing.T) {
	c := newClock()
	s := openStore(t, c)
	w := worker("d1", "thebot")

	a := send(t, s, w, "task A")
	if err := s.CloseOpen("d1", "stopped"); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Minute)
	b := send(t, s, w, "task B")

	yes := true
	if got := outcome(t, s, "d1", a.TaskID, Outcome{Kind: OutcomeDone, Reply: "A is done", Checked: &yes}); got != "" {
		t.Fatalf("a closed task's outcome was recorded on %q", got)
	}

	db := get(t, s, b.TaskID)
	if db.Status != StatusRunning || db.Executions[0].FinalReply != "" || db.Executions[0].Checked != nil {
		t.Fatalf("task B was touched: status=%s reply=%q checked=%v", db.Status, db.Executions[0].FinalReply, db.Executions[0].Checked)
	}
	if da := get(t, s, a.TaskID); da.Status != StatusAbandoned {
		t.Fatalf("task A is %s, want it left as it ended (abandoned)", da.Status)
	}
}

// A report naming a task this worker never had closes nothing.
func TestAnUnknownTaskIDClosesNothing(t *testing.T) {
	c := newClock()
	s := openStore(t, c)
	b := send(t, s, worker("d1", "thebot"), "task B")

	if got := outcome(t, s, "d1", "tffffffffffffffff", Outcome{Kind: OutcomeDone, Reply: "x"}); got != "" {
		t.Fatalf("an unknown task's outcome was recorded on %q", got)
	}
	if ok, _ := s.AddStep("d1", "tffffffffffffffff", Step{Text: "$ ls"}); ok {
		t.Fatal("a step naming an unknown task was recorded")
	}
	if db := get(t, s, b.TaskID); db.Status != StatusRunning || len(db.Executions[0].Steps) != 0 {
		t.Fatalf("task B was touched: %s, %d steps", db.Status, len(db.Executions[0].Steps))
	}
}

// Another worker's task ID is not this worker's: nothing of it is recorded.
func TestATaskIDOfAnotherWorkerClosesNothing(t *testing.T) {
	c := newClock()
	s := openStore(t, c)
	other := send(t, s, worker("d2", "thebot"), "theirs")
	mine := send(t, s, worker("d1", "thebot"), "mine")

	if got := outcome(t, s, "d1", other.TaskID, Outcome{Kind: OutcomeDone, Reply: "x"}); got != "" {
		t.Fatalf("recorded on %q", got)
	}
	if get(t, s, other.TaskID).Status != StatusRunning || get(t, s, mine.TaskID).Status != StatusRunning {
		t.Fatal("a task was closed by another worker's report")
	}
}

// A step for a task that ended moments ago is still that task's (its last line trails the reply); it is never moved
// onto the task that is live now.
func TestALateStepStaysOnItsOwnTask(t *testing.T) {
	c := newClock()
	s := openStore(t, c)
	w := worker("d1", "thebot")

	a := send(t, s, w, "task A")
	outcome(t, s, "d1", a.TaskID, Outcome{Kind: OutcomeDone, Reply: "A done"})
	b := send(t, s, w, "task B")

	c.advance(time.Second)
	if ok, _ := s.AddStep("d1", a.TaskID, Step{Text: "wrote a.log"}); !ok {
		t.Fatal("a trailing step was dropped")
	}
	if got := len(get(t, s, a.TaskID).Executions[0].Steps); got != 1 {
		t.Fatalf("task A has %d steps, want 1", got)
	}
	if got := len(get(t, s, b.TaskID).Executions[0].Steps); got != 0 {
		t.Fatalf("task B got %d of A's steps", got)
	}
}

// LiveWorkers lists the workers with a queued, running or waiting execution - what the gone-desktop reconcile
// closes when their container is gone (C-17) - and none whose tasks all ended.
func TestLiveWorkersListsOnlyWorkersWithOpenTasks(t *testing.T) {
	c := newClock()
	s := openStore(t, c)
	send(t, s, worker("busy", "thebot"), "still going")
	ended := send(t, s, worker("idle", "thebot"), "finished")
	outcome(t, s, "idle", ended.TaskID, Outcome{Kind: OutcomeDone, Reply: "ok"})

	live, err := s.LiveWorkers()
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0] != "busy" {
		t.Fatalf("LiveWorkers = %v, want [busy]", live)
	}

	if err := s.CloseOpen("busy", "desktop gone"); err != nil {
		t.Fatal(err)
	}
	if live, _ := s.LiveWorkers(); len(live) != 0 {
		t.Fatalf("after CloseOpen LiveWorkers = %v, want none", live)
	}
}
