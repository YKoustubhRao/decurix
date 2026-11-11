// Tests for a paused job as data (persist.go): a checkpoint encoded and decoded resumes as the one kept in memory
// does, and the encoding never holds the checkpoint's secret values.
package scheduler

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"thebot/internal/llm"
	"thebot/internal/skills"
)

// roundTrip is cp encoded and decoded again; the decoded one encodes to the same bytes.
func roundTrip(t *testing.T, cp *Checkpoint) *Checkpoint {
	t.Helper()
	raw, err := EncodeCheckpoint(cp)
	if err != nil {
		t.Fatalf("EncodeCheckpoint: %v", err)
	}
	back, err := DecodeCheckpoint(raw)
	if err != nil {
		t.Fatalf("DecodeCheckpoint: %v", err)
	}
	again, err := EncodeCheckpoint(back)
	if err != nil {
		t.Fatalf("EncodeCheckpoint (decoded): %v", err)
	}
	if !bytes.Equal(raw, again) {
		t.Fatalf("a decoded checkpoint encodes differently:\n%s\n%s", raw, again)
	}
	return back
}

// An operator goal paused on a question resumes from its decoded checkpoint with the same model calls and the same
// result as from the checkpoint in memory: its transcript and state came back whole.
func TestARestoredOperatorCheckpointResumesLikeTheOneInMemory(t *testing.T) {
	registerGraphFixtures()
	answer := func(m []llm.Message) string {
		u := lastUser(m)
		switch {
		case strings.Contains(u, "The human replied: beta"):
			return "DONE:\nArchived beta."
		case strings.Contains(u, "before running a single command"):
			return "HELP:\nThere are two projects, alpha and beta. Which one?"
		}
		return "HELP:\nWhich project?"
	}
	provider := &funcProvider{fn: answer}
	sched := newTestScheduler(t, provider)

	job, _ := sched.Run(context.Background(), "archive the project folder in ~/work", nil, func(string) {})
	if job.Outcome != OutcomeNeedsInput || job.Checkpoint == nil {
		t.Fatalf("job = %+v, want the operator's question", job)
	}
	restored := roundTrip(t, job.Checkpoint)

	provider.mu.Lock()
	provider.calls = nil
	provider.mu.Unlock()
	fromDisk, _ := sched.Execute(context.Background(), Request{Prompt: "beta", Resume: restored}, func(string) {})
	provider.mu.Lock()
	diskCalls := provider.calls
	provider.calls = nil
	provider.mu.Unlock()

	inMemory, _ := sched.Execute(context.Background(), Request{Prompt: "beta", Resume: job.Checkpoint}, func(string) {})
	provider.mu.Lock()
	memCalls := provider.calls
	provider.mu.Unlock()

	if fromDisk.Outcome != inMemory.Outcome || fromDisk.FinalAnswer != inMemory.FinalAnswer {
		t.Fatalf("restored: %s %q; in memory: %s %q", fromDisk.Outcome, fromDisk.FinalAnswer, inMemory.Outcome, inMemory.FinalAnswer)
	}
	if inMemory.Outcome != OutcomeAnswered || inMemory.FinalAnswer != "Archived beta." {
		t.Fatalf("in memory = %s %q", inMemory.Outcome, inMemory.FinalAnswer)
	}
	if !reflect.DeepEqual(diskCalls, memCalls) {
		t.Fatalf("the restored goal sent the model %d call(s), the one in memory %d, or not the same ones", len(diskCalls), len(memCalls))
	}
}

// A command paused for approval (a step with no checkpoint of its own) resumes from its decoded checkpoint: a yes runs
// it, and the original request is the job's.
func TestARestoredApprovalRunsTheApprovedCommand(t *testing.T) {
	registerGraphFixtures()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\necho pushed-for-real\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "git.cmd"), []byte("@echo pushed-for-real\r\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	sched := newTestScheduler(t, &mockProvider{})

	job, _ := sched.Run(context.Background(), "run this command: `git push origin main`", nil, func(string) {})
	if job.Outcome != OutcomeNeedsApproval || job.Checkpoint == nil {
		t.Fatalf("job = %+v, want a paused approval", job)
	}
	restored := roundTrip(t, job.Checkpoint)
	if cmd, _, ok := restored.Approval(); !ok || cmd != "git push origin main" {
		t.Fatalf("restored approval is for %q (%v)", cmd, ok)
	}
	notAYes, _ := sched.Execute(context.Background(), Request{Prompt: "what would that push?", Resume: roundTrip(t, job.Checkpoint)}, func(string) {})
	if strings.Contains(notAYes.FinalAnswer, "pushed-for-real") {
		t.Fatalf("a reply that is no yes ran the command: %+v", notAYes)
	}
	resumed, _ := sched.Execute(context.Background(), Request{Prompt: "yes", Resume: restored}, func(string) {})
	if resumed.Outcome != OutcomeAnswered || !strings.Contains(resumed.FinalAnswer, "pushed-for-real") {
		t.Fatalf("resumed = %+v, want the approved push to have run", resumed)
	}
	if resumed.Prompt != "run this command: `git push origin main`" {
		t.Errorf("resumed prompt = %q", resumed.Prompt)
	}
}

// Two paused steps (an approval, then a question whose skill keeps a text checkpoint) are both asked and the job
// finishes when every pause in between goes through the encoding.
func TestTwoPausedStepsSurviveTheEncodingBetweenEachReply(t *testing.T) {
	registerGraphFixtures()
	registerAskFixture()
	provider := &funcProvider{fn: func(m []llm.Message) string {
		if strings.Contains(lastUser(m), "Plan the request below") {
			return "1. fx-pay acme\n2. fx-ask which (independent)"
		}
		return "Combined reply."
	}}
	sched := newTestScheduler(t, provider)
	sched.MaxParallel = 3
	paidBefore := payments.Load()

	job, _ := sched.Run(context.Background(), "run fx work and then write the fx answer to a file", nil, func(string) {})
	asked := map[Outcome]bool{}
	for round := 0; round < 3 && job.Checkpoint != nil; round++ {
		asked[job.Outcome] = true
		job, _ = sched.Execute(context.Background(), Request{Prompt: "yes", Resume: roundTrip(t, job.Checkpoint)}, func(string) {})
	}
	if !asked[OutcomeNeedsApproval] || !asked[OutcomeNeedsInput] || job.Outcome != OutcomeAnswered {
		t.Fatalf("asked %v, ended %s %q", asked, job.Outcome, job.FinalAnswer)
	}
	if got := payments.Load() - paidBefore; got != 1 {
		t.Errorf("paid %d time(s), want once", got)
	}
}

// The encoding has no place for a checkpoint's secret values: none of them is in it, in any form.
func TestAnEncodedCheckpointNeverHoldsItsSecrets(t *testing.T) {
	const secret = "Zq9-hunter-secret-77"
	cp := &Checkpoint{Prompt: "log in", Kind: pauseInput, Question: "Which account?", Secrets: []string{secret, "pw:" + secret}}
	raw, err := EncodeCheckpoint(cp)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(secret)) || bytes.Contains(bytes.ToLower(raw), []byte("secrets")) {
		t.Fatalf("the encoding holds the checkpoint's secrets: %s", raw)
	}
	back, err := DecodeCheckpoint(raw)
	if err != nil || len(back.Secrets) != 0 {
		t.Fatalf("decoded %+v (%v), want no secrets", back, err)
	}
}

// A step whose skill keeps a checkpoint of a kind that cannot be encoded makes the whole checkpoint refuse to encode:
// it stays in memory only, never written in part.
func TestACheckpointOfAnUnknownKindIsNotEncoded(t *testing.T) {
	cp := &Checkpoint{Kind: pauseInput, nodes: []node{{result: skills.Result{Checkpoint: struct{ X int }{1}}}}}
	if _, err := EncodeCheckpoint(cp); err == nil {
		t.Fatal("encoded a checkpoint of a kind no decoder knows")
	}
}

// A result's Meta comes back in the shapes Go code reads it in: a whole number as an int, a list of strings as one.
func TestARestoredResultsMetaKeepsItsShapes(t *testing.T) {
	cp := &Checkpoint{Kind: pauseInput, nodes: []node{{hasResult: true, result: skills.Result{Meta: map[string]any{
		"exitCode": 3, "commands": []string{"ls", "pwd"}, "action": "x", "fatal": true}}}}}
	back := roundTrip(t, cp)
	meta := back.nodes[0].result.Meta
	if code, ok := meta["exitCode"].(int); !ok || code != 3 {
		t.Errorf("exitCode = %#v", meta["exitCode"])
	}
	if cmds, ok := meta["commands"].([]string); !ok || len(cmds) != 2 {
		t.Errorf("commands = %#v", meta["commands"])
	}
	if fatal, ok := meta["fatal"].(bool); !ok || !fatal {
		t.Errorf("fatal = %#v", meta["fatal"])
	}
}
