package handoff

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// A report that names a file on the receiver copies nothing back - verified or not; only files saved in the
// handoff's out folder return, and only with a verified result (C-06).
func TestOnlyAVerifiedResultsOutFolderComesBack(t *testing.T) {
	for _, tc := range []struct {
		name    string
		checked *bool
		back    bool
	}{
		{"unverified", no(), false},
		{"not said", nil, false},
		{"verified", yes(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			h, err := r.svc.Delegate(context.Background(), DelegateRequest{FromWorker: "a", ToWorker: "b", Request: "summarise"})
			if err != nil {
				t.Fatal(err)
			}
			r.fs.files["b"][home+"/clients/another-ledger.xlsx"] = []byte("not this worker's")
			r.fs.files["b"][OutDir(h.ID)+"/summary.txt"] = []byte("total 3")
			r.tasks.set(h.TaskID, TaskOutcome{Status: tasks.StatusDone, Checked: tc.checked,
				Report: "I looked at /home/headless/clients/another-ledger.xlsx and wrote " + OutDir(h.ID) + "/summary.txt"})

			got, _ := r.svc.Get(context.Background(), h.ID)
			for _, p := range r.fs.reads {
				if strings.Contains(p, "another-ledger") {
					t.Fatalf("a file the report named was read: %v", r.fs.reads)
				}
			}
			written := r.fs.written["a"+ReturnDir(h.ID)]
			if tc.back {
				if len(written) != 1 || string(written[0].Data) != "total 3" || len(got.Result.Files) != 1 {
					t.Fatalf("verified result: written %+v, files %v", written, got.Result.Files)
				}
			} else if len(written) != 0 || len(got.Result.Files) != 0 {
				t.Fatalf("an unverified result handed back %+v (%v)", written, got.Result.Files)
			}
		})
	}
}

// The receiver is told where to save what it makes.
func TestTheRequestNamesTheOutFolder(t *testing.T) {
	r := newRig(t)
	h, err := r.svc.Delegate(context.Background(), DelegateRequest{FromWorker: "a", ToWorker: "b", Request: "summarise"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.submit.calls[0].content, OutDir(h.ID)) {
		t.Fatalf("request does not name %s:\n%s", OutDir(h.ID), r.submit.calls[0].content)
	}
}

// A receiving task that ended while waiting on a person ("query", terminal) fails the handoff; one still waiting
// keeps it open, until the delegate ceiling (C-07).
func TestATaskEndedWaitingFailsTheHandoff(t *testing.T) {
	r := newRig(t)
	h, _ := r.svc.Delegate(context.Background(), DelegateRequest{FromWorker: "a", ToWorker: "b", Request: "do it"})

	r.tasks.set(h.TaskID, TaskOutcome{Status: tasks.StatusWaiting})
	if got, _ := r.svc.Get(context.Background(), h.ID); !got.Open() || !strings.Contains(got.Reason, "waiting for the answer") {
		t.Fatalf("waiting: %s %q", got.Status, got.Reason)
	}

	r.tasks.set(h.TaskID, TaskOutcome{Status: tasks.StatusQuery})
	if got, _ := r.svc.Get(context.Background(), h.ID); got.Status != StatusFailed {
		t.Fatalf("ended waiting: %s %q", got.Status, got.Reason)
	}
}

func TestADelegateThatNeverFinishesFails(t *testing.T) {
	r := newRig(t)
	h, _ := r.svc.Delegate(context.Background(), DelegateRequest{FromWorker: "a", ToWorker: "b", Request: "do it"})
	r.tasks.set(h.TaskID, TaskOutcome{Status: tasks.StatusRunning})

	r.now = r.now.Add(defaultDelegateTimeout - time.Minute)
	if got, _ := r.svc.Get(context.Background(), h.ID); !got.Open() {
		t.Fatalf("failed before the ceiling: %q", got.Reason)
	}
	r.now = r.now.Add(2 * time.Minute)
	if got, _ := r.svc.Get(context.Background(), h.ID); got.Status != StatusFailed {
		t.Fatalf("after the ceiling: %s", got.Status)
	}
}

// blockingSubmit holds a submission until released, as a stopped desktop waking does.
type blockingSubmit struct {
	fakeSubmit
	entered chan struct{}
	release chan struct{}
}

func (b *blockingSubmit) SubmitHandoff(ctx context.Context, worker, content, dir string, files []File) (string, error) {
	b.entered <- struct{}{}
	<-b.release
	return b.fakeSubmit.SubmitHandoff(ctx, worker, content, dir, files)
}

// The service's lock is not held while a request is submitted: another handoff's status read goes on meanwhile, and
// the one being sent is claimed, so it is never sent twice (C-16).
func TestASlowSendDoesNotHoldEveryHandoff(t *testing.T) {
	r := newRig(t)
	other, _ := r.svc.Delegate(context.Background(), DelegateRequest{FromWorker: "c", ToWorker: "d", Request: "first"})

	slow := &blockingSubmit{fakeSubmit: fakeSubmit{busy: map[string]bool{}}, entered: make(chan struct{}, 1), release: make(chan struct{})}
	r.svc.Submit = slow

	var wg sync.WaitGroup
	wg.Add(1)
	var sent Handoff
	go func() {
		defer wg.Done()
		sent, _ = r.svc.Delegate(context.Background(), DelegateRequest{FromWorker: "a", ToWorker: "b", Request: "slow"})
	}()
	<-slow.entered

	done := make(chan struct{})
	go func() {
		r.svc.Get(context.Background(), other.ID)
		r.svc.Tick(context.Background()) // sees the slow one as sending: leaves it alone
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a status read waited behind a slow send")
	}

	close(slow.release)
	wg.Wait()
	if sent.Status != StatusRunning || len(slow.calls) != 1 {
		t.Fatalf("slow handoff = %s, sent %d times", sent.Status, len(slow.calls))
	}
}

// A handoff left "sending" by a control plane that stopped mid-send fails after the lease, and is not sent again.
func TestAHandoffLeftSendingFails(t *testing.T) {
	r := newRig(t)
	r.submit.busy["b"] = true
	h, _ := r.svc.Delegate(context.Background(), DelegateRequest{FromWorker: "a", ToWorker: "b", Request: "do it"})
	if ok, err := r.svc.Store.Claim(h.ID); !ok || err != nil {
		t.Fatalf("claim: %v %v", ok, err)
	}
	r.submit.busy["b"] = false

	r.now = r.now.Add(sendingLease + time.Minute)
	r.svc.Tick(context.Background())
	got, _ := r.svc.Store.Get(h.ID)
	if got.Status != StatusFailed || len(r.submit.calls) != 0 {
		t.Fatalf("status %s %q, sent %d", got.Status, got.Reason, len(r.submit.calls))
	}
}

// To a caller a handoff being sent is still queued: the agent's tool and the page know no other open state.
func TestASendingHandoffReadsAsQueued(t *testing.T) {
	raw, err := json.Marshal(Handoff{ID: "h1", Status: StatusSending})
	if err != nil || !strings.Contains(string(raw), `"status":"queued"`) {
		t.Fatalf("%s %v", raw, err)
	}
}
