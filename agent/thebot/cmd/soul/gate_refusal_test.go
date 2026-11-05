package main

import (
	"strings"
	"testing"
	"time"

	"thebot/internal/scheduler"
)

// The gate refuses to ask (its question could not be shown without the task's secrets): gateAsk says so, for the
// gate's explicit answer and for an older gate's allowed-false-with-no-request; an ordinary pending request is not a
// refusal, and neither is a gate that does not answer.
func TestGateAskReportsARefusal(t *testing.T) {
	for line, want := range map[string]bool{
		`{"allowed": false, "approved": false, "refused": true, "request": null, "by_reply": ""}`:     true,
		`{"allowed": false, "approved": false, "request": null, "by_reply": ""}`:                      true,
		`{"allowed": false, "request": {"id": "req-9", "fingerprint": "fp-9", "question": "May I?"}}`: false,
		`{"error": "no material"}`: false,
	} {
		fakeGate(t, line)
		cp := &scheduler.Checkpoint{Kind: "approval", Action: "git push origin main", WorkDir: "/work"}
		if got := gateAsk(cp); got != want {
			t.Errorf("%s: refused = %v, want %v", line, got, want)
		}
		if want && cp.GateRequest != "" {
			t.Errorf("%s: a refusal left a request %q", line, cp.GateRequest)
		}
	}
}

// A refusal is a no to that one action: the job does not wait on a question no reply could approve, the action does
// not run, and nothing is left paused.
func TestAGateRefusalIsANoToThatAction(t *testing.T) {
	fakeGit(t)
	calls := fakeGate(t, `{"allowed": false, "approved": false, "refused": true, "request": null, "by_reply": ""}`)
	s := newTestServer(t)
	id := postChat(t, s, "{\"prompt\":\"run this command: `git push origin main`\",\"sessionId\":\"desk\"}")
	job := waitJob(t, s, id, 10*time.Second)
	if job["outcome"] == string(scheduler.OutcomeNeedsApproval) {
		t.Fatalf("the job waits on a question the gate would not ask: %v", job)
	}
	if strings.Contains(job["finalAnswer"].(string), "pushed-for-real") {
		t.Fatalf("the refused action ran: %v", job["finalAnswer"])
	}
	s.jobsMu.Lock()
	logged := strings.Join(s.jobs[id].Log, "\n")
	s.jobsMu.Unlock()
	if !strings.Contains(logged, "approval gate refused to ask") {
		t.Fatalf("the refusal is not said:\n%s", logged)
	}
	if cp := s.takePaused("desk"); cp != nil {
		t.Fatalf("a refused action was left paused: %+v", cp)
	}
	if len(calls()) == 0 {
		t.Fatal("the gate was never asked")
	}
}
