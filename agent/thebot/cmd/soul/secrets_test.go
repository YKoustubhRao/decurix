package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"thebot/internal/memory"
	"thebot/internal/withheld"
)

// testSecret stands for a value the person marked secret (a made-up one, of no credential's shape, so only the job's
// own list can withhold it).
const testSecret = "plumtree-cabinet-41"

// A job told a secret value keeps no copy of it in L2 or anything soul serves (C14).
func TestAJobsSecretIsKeptNowhereAndServedNowhere(t *testing.T) {
	proxy := &fakeProxy{}
	s := newProxiedServer(t, proxy.start(t))
	prompt := "sign in to the staff portal with " + testSecret + " and tell me who you are"
	body, _ := json.Marshal(map[string]any{"prompt": prompt, "taskId": "t_sec", "secrets": []string{testSecret}})
	id := postChat(t, s, string(body))
	job := waitJob(t, s, id, 10*time.Second)

	raw, err := os.ReadFile(filepath.Join(s.ws.Root(), ".memory", "l2.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(testSecret)) || !bytes.Contains(raw, []byte("staff portal")) {
		t.Fatalf("l2.jsonl = %s", raw)
	}
	mem := serve(s, http.MethodGet, "/api/memory", "")
	if mem.Code != 200 || strings.Contains(mem.Body.String(), testSecret) {
		t.Fatalf("/api/memory (%d) serves the secret: %s", mem.Code, mem.Body.String())
	}
	snap, _ := json.Marshal(job)
	if bytes.Contains(snap, []byte(testSecret)) {
		t.Fatalf("the job's answer holds the secret: %s", snap)
	}
	s.jobsMu.Lock()
	logged := strings.Join(s.jobs[id].Log, "\n")
	s.jobsMu.Unlock()
	if strings.Contains(logged, testSecret) {
		t.Fatalf("the job's log holds the secret:\n%s", logged)
	}
	// The model was sent the request as it is: the work needs the value.
	found := false
	for _, b := range proxy.bodies {
		found = found || bytes.Contains(b, []byte(testSecret))
	}
	if !found {
		t.Fatal("the model never saw the request's value")
	}
}

// A request that is secret as a whole keeps none of its text in L2.
func TestAWhollySecretRequestKeepsNoText(t *testing.T) {
	proxy := &fakeProxy{}
	s := newProxiedServer(t, proxy.start(t))
	body, _ := json.Marshal(map[string]any{"prompt": "hello " + testSecret, "secrets": []string{"hello " + testSecret}})
	waitJob(t, s, postChat(t, s, string(body)), 10*time.Second)
	raw, err := os.ReadFile(filepath.Join(s.ws.Root(), ".memory", "l2.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var entry struct{ Prompt string }
	if err := json.Unmarshal(bytes.TrimSpace(raw), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Prompt != withheld.Mark || bytes.Contains(raw, []byte("hello")) {
		t.Fatalf("l2 = %s", raw)
	}
}

// A step's text leaves soul without the secret: the job log is what the agent turns into "Running: ..." steps.
func TestAStepsTextIsWithheldBeforeItIsEmitted(t *testing.T) {
	job := newJobRecord("j1", "p")
	job.secrets = withheld.Clean([]string{testSecret})
	ch, _ := job.subscribe()
	job.appendLog("skill:operator -> $ gui type e3 '" + testSecret + "'")
	got := <-ch
	if strings.Contains(got, testSecret) || strings.Contains(strings.Join(job.Log, ""), testSecret) ||
		!strings.Contains(got, "gui type e3") {
		t.Fatalf("step text = %q", got)
	}
}

// The prompt prefix holds: the same request with and without a secrets list sends the model byte-identical bodies.
func TestSecretsNeverChangeWhatTheModelIsSent(t *testing.T) {
	send := func(withSecrets bool) [][]byte {
		proxy := &fakeProxy{}
		s := newProxiedServer(t, proxy.start(t))
		req := map[string]any{"prompt": "what is the capital of France? " + testSecret}
		if withSecrets {
			req["secrets"] = []string{testSecret}
		}
		body, _ := json.Marshal(req)
		waitJob(t, s, postChat(t, s, string(body)), 10*time.Second)
		return proxy.bodies
	}
	plain, secret := send(false), send(true)
	if len(plain) == 0 || len(plain) != len(secret) {
		t.Fatalf("%d calls without secrets, %d with", len(plain), len(secret))
	}
	for i := range plain {
		if !bytes.Equal(plain[i], secret[i]) {
			t.Fatalf("call %d differs with a secrets list:\n%s\n%s", i, plain[i], secret[i])
		}
	}
}

// The memory API withholds every secret value soul was told, from what an earlier job left in a store.
func TestTheMemoryAPIIsASecondWall(t *testing.T) {
	s := newTestServer(t)
	s.secrets.add([]string{testSecret})
	if err := s.sched.L2.Append(l2Line("enter " + testSecret + " in the form")); err != nil {
		t.Fatal(err)
	}
	mem := serve(s, http.MethodGet, "/api/memory", "")
	if strings.Contains(mem.Body.String(), testSecret) || !strings.Contains(mem.Body.String(), withheld.Mark) {
		t.Fatalf("/api/memory = %s", mem.Body.String())
	}
}

func l2Line(prompt string) memory.L2Entry { return memory.L2Entry{Prompt: prompt, Status: "answered"} }
