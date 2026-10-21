package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"thebot/internal/config"
	"thebot/internal/llm"
	"thebot/internal/memory"
	"thebot/internal/scheduler"
	"thebot/internal/skills"
	"thebot/internal/workspace"
)

// skills.Register uses a package-level global registry, so tests across
// this file must register the real terminal skill exactly once rather
// than accumulating harmless-but-messy duplicates on every test.
var registerTerminalOnce sync.Once

func registerTerminalSkillOnce() {
	registerTerminalOnce.Do(func() {
		skills.Register(skills.NewTerminal())
	})
}

// stubProvider avoids depending on a real Ollama server for HTTP-layer
// tests - these test routing/JSON/SSE framing, not the LLM itself (that's
// covered separately in internal/llm and internal/scheduler).
type stubProvider struct{}

func (stubProvider) Name() string { return "stub" }
func (stubProvider) Chat(ctx context.Context, model string, messages []llm.Message, opts llm.Options) (llm.Result, error) {
	return llm.Result{Text: "DECISION: ASK_USER\nREASON: stub\nRETRY_STEP:"}, nil
}
func (stubProvider) IsReachable(ctx context.Context) bool { return true }

// serve sends one request to s and returns what it answered.
func serve(s *server, method, target, body string) *httptest.ResponseRecorder {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	rec := httptest.NewRecorder()
	s.mux().ServeHTTP(rec, httptest.NewRequest(method, target, r))
	return rec
}

// realEmbedder is a reachable Ollama at OLLAMA_HOST (or the local default), or skips the test saying why it needs one.
func realEmbedder(t *testing.T, why string) *llm.OllamaProvider {
	t.Helper()
	host := os.Getenv("OLLAMA_HOST")
	if host == "" {
		host = "http://127.0.0.1:11434"
	}
	embedder := llm.NewOllama(host)
	if !embedder.IsReachable(context.Background()) {
		t.Skipf("no real Ollama at %s - %s", host, why)
	}
	return embedder
}

func newTestServer(t *testing.T) *server {
	t.Helper()
	ws, err := workspace.New(t.TempDir())
	if err != nil {
		t.Fatalf("workspace.New() error = %v", err)
	}
	l2, err := memory.NewL2(ws.Root())
	if err != nil {
		t.Fatalf("memory.NewL2() error = %v", err)
	}
	registerTerminalSkillOnce()
	provider := stubProvider{}
	sched := scheduler.New(provider, llm.DefaultModels("stub-model"), ws, l2, nil, nil, nil)
	return newServer(config.Config{OllamaHost: "stub"}, provider, ws, sched)
}

func TestHandleHealth(t *testing.T) {
	s := newTestServer(t)
	rec := serve(s, http.MethodGet, "/api/health", "")

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if body["ollama"] != true {
		t.Errorf("ollama = %v, want true (stub provider is always reachable)", body["ollama"])
	}
}

func TestHandleChatRejectsEmptyPrompt(t *testing.T) {
	s := newTestServer(t)
	rec := serve(s, http.MethodPost, "/api/chat", `{"prompt":""}`)

	if rec.Code != 400 {
		t.Errorf("status = %d, want 400 for an empty prompt", rec.Code)
	}
}

func TestHandleChatRejectsGET(t *testing.T) {
	s := newTestServer(t)
	rec := serve(s, http.MethodGet, "/api/chat", "")

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

func TestHandleChatAndPollJob(t *testing.T) {
	s := newTestServer(t)

	id := postChat(t, s, `{"prompt":"run this command: `+"`echo http-test`"+`"}`)
	if id == "" {
		t.Fatal("no job id returned")
	}

	// Poll the real job record until the real background goroutine finishes -
	// no sleeping a fixed amount, wait on the real state.
	if status := waitJob(t, s, id, 5*time.Second)["status"]; status != "done" {
		t.Fatalf("job did not reach status=done within the deadline (last status: %q)", status)
	}

	// Now fetch it over the real HTTP handler, not the internal struct.
	rec2 := serve(s, http.MethodGet, "/api/jobs/"+id, "")
	if rec2.Code != 200 {
		t.Fatalf("GET /api/jobs/%s status = %d, want 200", id, rec2.Code)
	}
	var fetched map[string]any
	_ = json.Unmarshal(rec2.Body.Bytes(), &fetched)
	if !strings.Contains(fetched["finalAnswer"].(string), "http-test") {
		t.Errorf("finalAnswer = %v, expected it to contain the real command output", fetched["finalAnswer"])
	}
}

func TestUIServedAtRoot(t *testing.T) {
	s := newTestServer(t)
	rec := serve(s, http.MethodGet, "/", "")

	if rec.Code != 200 {
		t.Fatalf("GET / status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<title>TheBot</title>") {
		t.Error("GET / did not serve the embedded UI (missing expected <title>)")
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
		t.Errorf("Content-Type = %q, want text/html", rec.Header().Get("Content-Type"))
	}
}

func TestRAGStatsEndpoint(t *testing.T) {
	s := newTestServer(t)
	rec := serve(s, http.MethodGet, "/api/rag/stats", "")

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	// This test server has no RAG wired (nil-safe path) - real numbers,
	// not a crash, is the property under test.
	if body["totalChunks"] != float64(0) {
		t.Errorf("totalChunks = %v, want 0 for a scheduler with no RAG configured", body["totalChunks"])
	}
}

func TestFeedbackStatsEndpoint(t *testing.T) {
	s := newTestServer(t)
	rec := serve(s, http.MethodGet, "/api/feedback/stats", "")

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["negativeCount"] != float64(0) {
		t.Errorf("negativeCount = %v, want 0 for a scheduler with no feedback store configured", body["negativeCount"])
	}
}

func TestHandleMemoryNilSafeDefaults(t *testing.T) {
	s := newTestServer(t) // RAG and Feedback are nil in this test server
	rec := serve(s, http.MethodGet, "/api/memory", "")

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response not valid JSON: %v", err)
	}

	l2, _ := body["l2"].(map[string]any)
	if l2 == nil || l2["totalJobs"] != float64(0) {
		t.Errorf("l2.totalJobs = %v, want 0 for a fresh L2 log", l2)
	}
	rag, _ := body["rag"].(map[string]any)
	if rag == nil || rag["totalChunks"] != float64(0) || rag["totalFiles"] != float64(0) {
		t.Errorf("rag stats = %v, want zeroed defaults when no RAG is configured", rag)
	}
	fb, _ := body["feedback"].(map[string]any)
	if fb == nil || fb["totalEntries"] != float64(0) {
		t.Errorf("feedback.totalEntries = %v, want 0 when no feedback store is configured", fb)
	}

	// L1 must always be present, even unconfigured. It was omitted from
	// this response entirely while recall was demonstrably working, so the
	// memory drawer showed nothing for the one tier users open it to see.
	l1, _ := body["l1"].(map[string]any)
	if l1 == nil {
		t.Fatal("response has no l1 key at all - the L1 tier must always be reported")
	}
	if l1["totalFacts"] != float64(0) {
		t.Errorf("l1.totalFacts = %v, want 0 when no L1 is configured", l1["totalFacts"])
	}
	if _, ok := l1["recent"].([]any); !ok {
		t.Errorf("l1.recent = %v, want an array so the UI can always iterate it", l1["recent"])
	}
}

// The other half of the L1 fix: a real stored fact must actually reach
// the endpoint, with its content and provenance - not just a count.
func TestHandleMemoryReportsRealL1Facts(t *testing.T) {
	embedder := realEmbedder(t, "storing an L1 fact needs real embeddings")

	s := newTestServer(t)
	l1, err := memory.NewL1(s.ws.Root(), stubProvider{}, embedder, "stub-model")
	if err != nil {
		t.Fatalf("memory.NewL1() error = %v", err)
	}
	defer l1.Close()
	s.sched.L1 = l1

	if err := l1.Store(context.Background(), "The user's name is Divyanshu Shekhar.", "explicit"); err != nil {
		t.Fatalf("Store() error = %v", err)
	}

	rec := serve(s, http.MethodGet, "/api/memory", "")

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response not valid JSON: %v", err)
	}
	l1Info, _ := body["l1"].(map[string]any)
	if l1Info == nil || l1Info["totalFacts"] != float64(1) {
		t.Fatalf("l1.totalFacts = %v, want 1 after storing one fact", l1Info["totalFacts"])
	}
	recent, _ := l1Info["recent"].([]any)
	if len(recent) != 1 {
		t.Fatalf("l1.recent has %d entries, want 1", len(recent))
	}
	got, _ := recent[0].(map[string]any)
	if !strings.Contains(got["fact"].(string), "Divyanshu") {
		t.Errorf("l1.recent[0].fact = %v, want the real stored fact", got["fact"])
	}
	if got["source"] != "explicit" {
		t.Errorf("l1.recent[0].source = %v, want explicit so the UI can say the user asked for it", got["source"])
	}
	if got["createdAt"] == "" {
		t.Error("l1.recent[0].createdAt is empty, want a real timestamp")
	}
}

func TestHandleMemoryReflectsRealL2Activity(t *testing.T) {
	s := newTestServer(t)

	id := postChat(t, s, `{"prompt":"run this command: `+"`echo memory-test`"+`"}`)
	waitJob(t, s, id, 5*time.Second)

	rec2 := serve(s, http.MethodGet, "/api/memory", "")
	var body map[string]any
	_ = json.Unmarshal(rec2.Body.Bytes(), &body)

	l2, _ := body["l2"].(map[string]any)
	if l2["totalJobs"] != float64(1) {
		t.Errorf("l2.totalJobs = %v, want 1 real job after running one", l2["totalJobs"])
	}
	recent, _ := l2["recent"].([]any)
	if len(recent) != 1 {
		t.Fatalf("l2.recent has %d entries, want 1", len(recent))
	}
	entry, _ := recent[0].(map[string]any)
	if !strings.Contains(entry["prompt"].(string), "memory-test") {
		t.Errorf("recent entry prompt = %v, expected it to contain the real prompt", entry["prompt"])
	}
}

func TestHandleJobsUnknownID(t *testing.T) {
	s := newTestServer(t)
	rec := serve(s, http.MethodGet, "/api/jobs/does-not-exist", "")
	if rec.Code != 404 {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestStreamAfterCompletion is the real HTTP-level version of the
// regression test in main_test.go - proves the fix end to end through the
// actual SSE handler, not just the jobRecord type in isolation.
func TestStreamAfterCompletion(t *testing.T) {
	s := newTestServer(t)

	id := postChat(t, s, `{"prompt":"run this command: `+"`echo stream-after-done`"+`"}`)
	waitJob(t, s, id, 5*time.Second)

	// Real HTTP server so the streaming response can actually be read
	// incrementally - httptest.ResponseRecorder doesn't support that.
	realSrv := httptest.NewServer(s.mux())
	defer realSrv.Close()

	resp, err := http.Get(realSrv.URL + "/api/jobs/" + id + "/stream")
	if err != nil {
		t.Fatalf("GET stream error = %v", err)
	}
	defer resp.Body.Close()

	sawFinal := false
	sawRawSentinel := false
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if strings.Contains(payload, "__DONE__") {
			sawRawSentinel = true
		}
		var evt map[string]any
		_ = json.Unmarshal([]byte(payload), &evt)
		if evt["type"] == "final" {
			sawFinal = true
			break
		}
	}

	if sawRawSentinel {
		t.Error("the raw completion sentinel leaked into the SSE stream - the exact bug this guards against")
	}
	if !sawFinal {
		t.Error("never received a type=final event for a job that connected after it already completed")
	}
}

// The real point of the whole skill library exposed over HTTP: a pending
// proposal is listed but never confirmed, and confirming it is the ONLY
// thing that moves it into real use.
func TestHandleSkillsListsAndConfirms(t *testing.T) {
	embedder := realEmbedder(t, "skill matching needs real embeddings")

	s := newTestServer(t)
	lib, err := memory.NewSkillLibrary(s.ws.Root(), embedder, "stub-model")
	if err != nil {
		t.Fatalf("NewSkillLibrary() error = %v", err)
	}
	s.sched.SkillLib = lib

	proposed, err := lib.Propose("a real trigger", []string{"do a real thing"}, "research")
	if err != nil {
		t.Fatalf("Propose() error = %v", err)
	}

	rec := serve(s, http.MethodGet, "/api/skills", "")
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response not valid JSON: %v", err)
	}
	pending, _ := body["pending"].([]any)
	if len(pending) != 1 {
		t.Fatalf("pending = %v, want exactly 1 real proposal", pending)
	}
	confirmed, _ := body["confirmed"].([]any)
	if len(confirmed) != 0 {
		t.Fatalf("confirmed = %v, want empty before any confirmation", confirmed)
	}

	confirmRec := serve(s, http.MethodPost, "/api/skills/"+proposed.Slug+"/confirm", "")
	if confirmRec.Code != 200 {
		t.Fatalf("confirm status = %d, body=%s", confirmRec.Code, confirmRec.Body.String())
	}

	rec2 := serve(s, http.MethodGet, "/api/skills", "")
	var body2 map[string]any
	_ = json.Unmarshal(rec2.Body.Bytes(), &body2)
	confirmed2, _ := body2["confirmed"].([]any)
	pending2, _ := body2["pending"].([]any)
	if len(confirmed2) != 1 {
		t.Errorf("confirmed = %v after confirming, want exactly 1", confirmed2)
	}
	if len(pending2) != 0 {
		t.Errorf("pending = %v after confirming, want empty (moved out of pending)", pending2)
	}
}

// Rejecting a proposal must discard it for good - it must never show up
// as pending OR confirmed afterward.
func TestHandleSkillsReject(t *testing.T) {
	s := newTestServer(t)
	lib, err := memory.NewSkillLibrary(s.ws.Root(), nil, "stub-model")
	if err != nil {
		t.Fatalf("NewSkillLibrary() error = %v", err)
	}
	s.sched.SkillLib = lib

	proposed, err := lib.Propose("a trigger to reject", []string{"do a thing"}, "research")
	if err != nil {
		t.Fatalf("Propose() error = %v", err)
	}

	rec := serve(s, http.MethodPost, "/api/skills/"+proposed.Slug+"/reject", "")
	if rec.Code != 200 {
		t.Fatalf("reject status = %d, body=%s", rec.Code, rec.Body.String())
	}

	pending, _ := lib.ListPending()
	if len(pending) != 0 {
		t.Errorf("ListPending() = %v after rejecting, want empty", pending)
	}
}

// With no skill library configured at all (e.g. no embedder available),
// the endpoint must degrade to real empty defaults, never crash.
func TestHandleSkillsNilSafeWithNoLibraryConfigured(t *testing.T) {
	s := newTestServer(t) // SkillLib is nil here
	rec := serve(s, http.MethodGet, "/api/skills", "")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response not valid JSON: %v", err)
	}
	if body["confirmed"] == nil || body["pending"] == nil {
		t.Errorf("body = %v, want real empty arrays even with no skill library configured", body)
	}
}

func postChat(t *testing.T, s *server, body string) string {
	t.Helper()
	rec := serve(s, http.MethodPost, "/api/chat", body)
	if rec.Code != 200 {
		t.Fatalf("POST /api/chat status = %d: %s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id, _ := created["id"].(string)
	return id
}

func waitJob(t *testing.T, s *server, id string, within time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		s.jobsMu.Lock()
		job := s.jobs[id]
		s.jobsMu.Unlock()
		if job != nil && job.isDone() {
			snap := job.snapshot()
			raw, _ := json.Marshal(snap)
			var out map[string]any
			_ = json.Unmarshal(raw, &out)
			return out
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s did not finish within %s", id, within)
	return nil
}

func fakeGit(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(bin+"/git", []byte("#!/bin/sh\necho pushed-for-real\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// What PowerShell runs for `git` on a Windows worker.
	if err := os.WriteFile(bin+"/git.cmd", []byte("@echo pushed-for-real\r\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// The whole approval round trip over HTTP: the job pauses and says why,
// and the human's "yes" in the same session carries on from the checkpoint
// rather than starting the request again.
func TestChatPausesForApprovalAndTheNextRequestInTheSessionResumesIt(t *testing.T) {
	fakeGit(t)
	s := newTestServer(t)

	paused := waitJob(t, s, postChat(t, s, `{"prompt":"run this command: `+"`git push origin main`"+`","sessionId":"desk"}`), 5*time.Second)
	if paused["outcome"] != "needs_approval" || !strings.Contains(paused["question"].(string), "git push origin main") {
		t.Fatalf("first job = %v, want a paused approval naming the command", paused)
	}

	resumed := waitJob(t, s, postChat(t, s, `{"prompt":"yes","sessionId":"desk","reply":true}`), 5*time.Second)
	if resumed["resumed"] != true || resumed["outcome"] != "answered" || !strings.Contains(resumed["finalAnswer"].(string), "pushed-for-real") {
		t.Fatalf("second job = %v, want the approved command to have run", resumed)
	}
}

func TestChatDropsAPausedJobWhenTheHostSaysItIsNotAReply(t *testing.T) {
	fakeGit(t)
	s := newTestServer(t)
	waitJob(t, s, postChat(t, s, `{"prompt":"run this command: `+"`git push origin main`"+`","sessionId":"desk"}`), 5*time.Second)

	next := waitJob(t, s, postChat(t, s, `{"prompt":"run this command: `+"`echo fresh-request`"+`","sessionId":"desk","reply":false}`), 5*time.Second)
	if next["resumed"] == true || !strings.Contains(next["finalAnswer"].(string), "fresh-request") {
		t.Fatalf("job = %v, want a fresh request, not a resume", next)
	}
}

func TestCancelStopsARunningJob(t *testing.T) {
	s := newTestServer(t)
	id := postChat(t, s, `{"prompt":"run this command: `+"`sleep 30`"+`"}`)
	time.Sleep(200 * time.Millisecond)

	rec := serve(s, http.MethodPost, "/api/jobs/"+id+"/cancel", "")
	if rec.Code != 200 {
		t.Fatalf("cancel status = %d", rec.Code)
	}

	job := waitJob(t, s, id, 5*time.Second)
	if job["outcome"] != "failed" || !strings.Contains(job["finalAnswer"].(string), "cancelled") {
		t.Errorf("job = %v, want a cancelled failure", job)
	}
}

// The host's context must never be recorded as what the human said.
func TestChatKeepsHostContextOutOfSessionHistory(t *testing.T) {
	s := newTestServer(t)
	waitJob(t, s, postChat(t, s, `{"prompt":"run this command: `+"`echo hi`"+`","sessionId":"desk","context":"IMPORTANT SAFETY RULE: ..."}`), 5*time.Second)
	for _, m := range s.sessions.History("desk") {
		if strings.Contains(m.Content, "SAFETY RULE") {
			t.Errorf("session history recorded the host context: %+v", m)
		}
	}
}
