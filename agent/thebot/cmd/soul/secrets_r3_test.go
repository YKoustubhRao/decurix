package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"thebot/internal/scheduler"
	"thebot/internal/withheld"
)

// B1: a job's own value is withheld however many values the process was told before, longer or of the same length.
func TestAJobsOwnSecretIsNeverCutForRoom(t *testing.T) {
	proxy := &fakeProxy{}
	s := newProxiedServer(t, proxy.start(t))
	for i := 0; i < 300; i++ {
		s.secrets.add(fmt.Sprintf("earlier-whole-request-number-%03d", i), fmt.Sprintf("%06d", 100000+i))
	}
	body, _ := json.Marshal(map[string]any{"prompt": "the code is 999999, sign in with it", "secrets": []string{"999999"}})
	id := postChat(t, s, string(body))
	job := waitJob(t, s, id, 10*time.Second)
	raw, _ := os.ReadFile(filepath.Join(s.ws.Root(), ".memory", "l2.jsonl"))
	snap, _ := json.Marshal(job)
	s.jobsMu.Lock()
	logged := strings.Join(s.jobs[id].Log, "\n")
	s.jobsMu.Unlock()
	if bytes.Contains(raw, []byte("999999")) || bytes.Contains(snap, []byte("999999")) || strings.Contains(logged, "999999") {
		t.Fatalf("the job's own value leaked after 600 earlier values: l2 %s", raw)
	}
}

// B1: another job's value never rewrites this job's answer.
func TestAnEarlierJobsValueLeavesALaterAnswerAlone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "Total: 41110.00"}}}})
	}))
	defer srv.Close()
	s := newProxiedServer(t, srv.URL)
	first, _ := json.Marshal(map[string]any{"prompt": "my PIN is 4111, remember nothing", "secrets": []string{"4111"}})
	waitJob(t, s, postChat(t, s, string(first)), 10*time.Second)
	later := waitJob(t, s, postChat(t, s, `{"prompt":"run this command: `+"`echo Total-41110.00`"+`"}`), 10*time.Second)
	if answer, _ := later["finalAnswer"].(string); !strings.Contains(answer, "Total-41110.00") {
		t.Fatalf("a later job's answer was rewritten by an earlier job's value: %q", answer)
	}
}

// B1 and B2: a paused job's own values are withheld when the next request in its session quotes its prompt, and
// before the prompt is cut or quoted: a wholly secret request longer than the cut, over several lines, leaves no part.
func TestAPausedJobsPromptIsWithheldBeforeItIsQuoted(t *testing.T) {
	whole := "please sign in to the staff portal as mira.k\nusing the password plumtree-cabinet-41\nand download the March statement"
	for _, reply := range []bool{false, true} {
		s := newTestServer(t)
		s.storePaused("desk", &scheduler.Checkpoint{Prompt: whole, Secrets: []string{whole}, CreatedAt: time.Now()})
		body, _ := json.Marshal(map[string]any{"prompt": "a new request", "sessionId": "desk", "reply": reply})
		id := postChat(t, s, string(body))
		waitJob(t, s, id, 10*time.Second)
		s.jobsMu.Lock()
		logged := strings.Join(s.jobs[id].Log, "\n")
		s.jobsMu.Unlock()
		for _, part := range []string{"staff portal", "plumtree", "mira.k", `\n`} {
			if strings.Contains(logged, part) {
				t.Fatalf("reply=%v: the paused prompt's %q left in a step:\n%s", reply, part, logged)
			}
		}
		if !strings.Contains(logged, withheld.Mark) {
			t.Fatalf("reply=%v: no quoted prompt at all:\n%s", reply, logged)
		}
	}
}

// A value straddling the 80-byte cut of the quoted paused prompt leaves none of itself.
func TestAValueStraddlingTheCutLeavesNoPart(t *testing.T) {
	prompt := strings.Repeat("a", 70) + " plumtree-cabinet-41 and more after it"
	s := newTestServer(t)
	s.storePaused("desk", &scheduler.Checkpoint{Prompt: prompt, Secrets: []string{"plumtree-cabinet-41"}, CreatedAt: time.Now()})
	id := postChat(t, s, `{"prompt":"a new request","sessionId":"desk","reply":false}`)
	waitJob(t, s, id, 10*time.Second)
	s.jobsMu.Lock()
	logged := strings.Join(s.jobs[id].Log, "\n")
	s.jobsMu.Unlock()
	if strings.Contains(logged, "plum") {
		t.Fatalf("part of the value left with the cut:\n%s", logged)
	}
}

// A job sent more secret values than soul accepts is refused, never run with some of them cut.
func TestTooManySecretValuesAreRefused(t *testing.T) {
	s := newTestServer(t)
	many := make([]string, withheld.MaxValues+1)
	for i := range many {
		many[i] = fmt.Sprintf("value-%04d", i)
	}
	body, _ := json.Marshal(map[string]any{"prompt": "hello", "secrets": many})
	if rec := serve(s, http.MethodPost, "/api/chat", string(body)); rec.Code != 400 {
		t.Fatalf("status %d, want 400", rec.Code)
	}
}
