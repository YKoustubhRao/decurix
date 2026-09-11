package main

import (
	"testing"
	"time"
)

func TestJobRecordSubscribeBeforeFinish(t *testing.T) {
	job := newJobRecord("test1", "prompt")
	ch, backlog := job.subscribe()
	if len(backlog) != 0 {
		t.Errorf("backlog = %v, want empty for a fresh job", backlog)
	}

	job.appendLog("line one")
	select {
	case line := <-ch:
		if line != "line one" {
			t.Errorf("got %q, want %q", line, "line one")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for live log line")
	}

	if job.isDone() {
		t.Error("isDone() = true before finish() was called")
	}
	job.finish()
	if !job.isDone() {
		t.Error("isDone() = false after finish() was called")
	}

	select {
	case <-job.done:
		// correct - channel closed
	case <-time.After(time.Second):
		t.Fatal("done channel was not closed after finish()")
	}
}

// TestJobRecordSubscribeAfterFinish is the exact regression case for the
// real bug found via live testing: a client connecting to the stream
// AFTER the job already completed must still get a proper "done" signal
// (via isDone()), not have completion silently invisible to it - which is
// exactly what happened when completion was a magic "__DONE__" string
// mixed into the log line list instead of a real channel.
func TestJobRecordSubscribeAfterFinish(t *testing.T) {
	job := newJobRecord("test2", "prompt")
	job.appendLog("real work happened")
	job.finish()

	_, backlog := job.subscribe()
	if len(backlog) != 1 || backlog[0] != "real work happened" {
		t.Errorf("backlog = %v, want [\"real work happened\"]", backlog)
	}
	if !job.isDone() {
		t.Error("isDone() = false for a job that already finished before subscribing")
	}
	// Critically: the real log content must never contain a fake sentinel
	// value standing in for completion.
	for _, line := range backlog {
		if line == "__DONE__" {
			t.Error("backlog contains a magic completion sentinel as a real log line - the bug this test guards against")
		}
	}
}

func TestJobRecordFinishIsIdempotent(t *testing.T) {
	job := newJobRecord("test3", "prompt")
	job.finish()
	job.finish() // must not panic on double-close
	if !job.isDone() {
		t.Error("isDone() = false after finish() called twice")
	}
}

func TestJobRecordUnsubscribeStopsDelivery(t *testing.T) {
	job := newJobRecord("test4", "prompt")
	ch, _ := job.subscribe()
	job.unsubscribe(ch)

	job.appendLog("line after unsubscribe")
	select {
	case line, ok := <-ch:
		if ok {
			t.Errorf("received %q on an unsubscribed channel", line)
		}
	case <-time.After(100 * time.Millisecond):
		// correct - nothing delivered
	}
}
