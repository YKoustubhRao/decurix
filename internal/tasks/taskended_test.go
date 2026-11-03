package tasks

import (
	"slices"
	"sync"
	"testing"
)

// Every way a task closes is told to the OnTaskEnded hook once, with its worker: done, error, and the ones CloseOpen
// ends (a stop, a restart, a delete; a running task abandoned and a waiting one ended waiting). A task that only
// pauses for a person, or one that is still queued behind another, is not told. The hook runs outside the store's
// lock: it calls the store itself here, which would deadlock under it.
func TestEveryClosedTaskIsToldToTheHookOnce(t *testing.T) {
	s := openStore(t, newClock())
	var mu sync.Mutex
	var told []string
	s.OnTaskEnded(func(workerID, taskID string) {
		_, _ = s.Busy(workerID) // takes the store's lock
		mu.Lock()
		told = append(told, workerID+"/"+taskID)
		mu.Unlock()
	})
	saw := func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := append([]string(nil), told...)
		told = nil
		return out
	}

	box := worker("box", "ollama")
	done := send(t, s, box, "sum the column")
	queued := send(t, s, box, "then email it")
	outcome(t, s, "box", done.TaskID, Outcome{Kind: OutcomeDone, Reply: "Summed."})
	if got := saw(); !slices.Equal(got, []string{"box/" + done.TaskID}) {
		t.Fatalf("a done task told %v", got)
	}
	outcome(t, s, "box", queued.TaskID, Outcome{Kind: OutcomeError, Reply: "No mail account."})
	if got := saw(); !slices.Equal(got, []string{"box/" + queued.TaskID}) {
		t.Fatalf("a failed task told %v", got)
	}
	// A second end for a closed task is no second notice.
	outcome(t, s, "box", queued.TaskID, Outcome{Kind: OutcomeDone, Reply: "late"})
	if got := saw(); len(got) != 0 {
		t.Fatalf("an ended task was told again: %v", got)
	}

	asking := send(t, s, box, "buy the domain")
	outcome(t, s, "box", asking.TaskID, Outcome{Kind: OutcomeApproval, Reply: "Approval needed before I buy example.com"})
	if got := saw(); len(got) != 0 {
		t.Fatalf("a task waiting on a person was told as ended: %v", got)
	}
	if err := s.CloseOpen("box", "The desktop was stopped."); err != nil {
		t.Fatal(err)
	}
	if got := saw(); !slices.Equal(got, []string{"box/" + asking.TaskID}) {
		t.Fatalf("stopping a desktop waiting on a person told %v", got)
	}

	other := worker("other", "claude")
	running := send(t, s, other, "build the project")
	behind := send(t, s, other, "then deploy it")
	if running.TaskID == behind.TaskID {
		t.Fatal("the second request did not queue as its own task")
	}
	if err := s.CloseOpen("other", "The desktop was deleted."); err != nil {
		t.Fatal(err)
	}
	got := saw()
	slices.Sort(got)
	want := []string{"other/" + running.TaskID, "other/" + behind.TaskID}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("deleting a desktop told %v, want %v (running and queued)", got, want)
	}
	if err := s.CloseOpen("other", "again"); err != nil {
		t.Fatal(err)
	}
	if got := saw(); len(got) != 0 {
		t.Fatalf("a close with nothing open told %v", got)
	}
}
