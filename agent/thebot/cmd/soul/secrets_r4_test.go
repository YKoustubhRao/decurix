package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// R2: a job told a value pauses; the reply that resumes it comes with no secrets (the agent restarted in between, or
// sent none). The paused job's values ride on its checkpoint, so the resumed job's log, its answer and L2 still hold
// no copy.
func TestAPausedJobsValuesSurviveAReplyWithoutThem(t *testing.T) {
	fakeGit(t)
	s := newTestServer(t)
	const v = "plumtree-cabinet-41"
	first, _ := json.Marshal(map[string]any{"prompt": "run this command: `git push origin " + v + "`",
		"sessionId": "desk", "secrets": []string{v}})
	paused := waitJob(t, s, postChat(t, s, string(first)), 10*time.Second)
	if paused["outcome"] != "needs_approval" {
		t.Fatalf("the first job did not pause: %v", paused)
	}

	id := postChat(t, s, `{"prompt":"yes","sessionId":"desk","reply":true}`)
	resumed := waitJob(t, s, id, 10*time.Second)
	if resumed["resumed"] != true || !strings.Contains(resumed["finalAnswer"].(string), "pushed-for-real") {
		t.Fatalf("the reply did not resume and run it: %v", resumed)
	}
	s.jobsMu.Lock()
	logged := strings.Join(s.jobs[id].Log, "\n")
	s.jobsMu.Unlock()
	snap, _ := json.Marshal(resumed)
	raw, _ := os.ReadFile(filepath.Join(s.ws.Root(), ".memory", "l2.jsonl"))
	if strings.Contains(logged, v) {
		t.Fatalf("the resumed job's log holds the paused job's value:\n%s", logged)
	}
	if bytes.Contains(snap, []byte(v)) {
		t.Fatalf("the resumed job's answer holds it: %s", snap)
	}
	if bytes.Contains(raw, []byte(v)) {
		t.Fatalf("L2 holds it: %s", raw)
	}
	if !strings.Contains(logged, "answers the paused job") {
		t.Fatalf("the resume was not logged at all:\n%s", logged)
	}
}
