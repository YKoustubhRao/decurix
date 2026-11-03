package skills

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeExtServer is ext_server.py's request inbox with no extension behind it:
// requests are accepted and never claimed, unless claim says otherwise.
type fakeExtServer struct {
	mu        sync.Mutex
	status    string
	result    *extResult
	cancelled bool
	submitted map[string]any
}

func (f *fakeExtServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/requests", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.submitted = nil
		json.NewDecoder(r.Body).Decode(&f.submitted)
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]string{"id": "r1"})
	})
	mux.HandleFunc("/requests/status", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		json.NewEncoder(w).Encode(extRequestsStatus{Requests: []extRequest{{ID: "r1", Status: f.status, Result: f.result}}})
	})
	mux.HandleFunc("/requests/r1/cancel", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.cancelled = true
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	return mux
}

// Measured live: the desktop's Chromium ran without the extension, the
// request sat pending, and the task waited out 16 minutes. Unclaimed, it is
// withdrawn and the operator does the task on the screen.
func TestBrowserSkillHandsOverWhenNoExtensionTakesTheTask(t *testing.T) {
	ext := &fakeExtServer{status: "pending"}
	server := httptest.NewServer(ext.handler())
	defer server.Close()

	skill := NewBrowser()
	skill.BaseURL = server.URL
	skill.PickupTimeout = 50 * time.Millisecond

	started := time.Now()
	res, err := skill.Run(RunContext{Ctx: context.Background(), OnLog: func(string) {}}, Subtask{Step: "open example.com and search for x", Type: "browser"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Verified || res.Meta["reroute"] != "operator" {
		t.Fatalf("result = %+v, want a reroute to the operator", res)
	}
	if !ext.cancelled {
		t.Error("the unclaimed request was left in the inbox for a later listener to act on")
	}
	if waited := time.Since(started); waited > 10*time.Second {
		t.Errorf("waited %s for a request nobody claimed", waited)
	}
}

// A request the extension took is waited for as before, however long it runs.
func TestBrowserSkillWaitsForARequestTheExtensionTook(t *testing.T) {
	ext := &fakeExtServer{status: "running"}
	server := httptest.NewServer(ext.handler())
	defer server.Close()

	go func() {
		time.Sleep(2500 * time.Millisecond)
		ext.mu.Lock()
		ext.status, ext.result = "done", &extResult{Status: "done", Answer: "Found it."}
		ext.mu.Unlock()
	}()

	skill := NewBrowser()
	skill.BaseURL = server.URL
	skill.PickupTimeout = 50 * time.Millisecond

	res, _ := skill.Run(RunContext{Ctx: context.Background(), OnLog: func(string) {}}, Subtask{Step: "search", Type: "browser"})
	if !res.Verified || !strings.Contains(res.Output, "Found it.") || ext.cancelled {
		t.Fatalf("result = %+v (cancelled %v), want the extension's answer", res, ext.cancelled)
	}
}

// The extension's approval gate stopped before a Send: the step pauses for a
// person, and only their yes to that action goes back with the rerun.
func TestBrowserSkillAsksBeforeWhatTheGateStopped(t *testing.T) {
	send := `click "Send" on mail.google.com`
	ext := &fakeExtServer{status: "done", result: &extResult{Status: "needs_approval",
		Approval: &browserCheckpoint{Action: send, Reason: `"Send" sends, pays, buys or publishes something on your behalf`}}}
	server := httptest.NewServer(ext.handler())
	defer server.Close()

	skill := NewBrowser()
	skill.BaseURL = server.URL
	step := Subtask{Step: "reply to Priya", Type: "browser"}

	res, _ := skill.Run(RunContext{Ctx: context.Background(), OnLog: func(string) {}}, step)
	if res.Verified || res.Meta["needsApproval"] != true || res.Meta["action"] != send {
		t.Fatalf("result = %+v, want a pause for approval of %q", res, send)
	}
	if _, ok := ext.submitted["approved"]; ok {
		t.Fatalf("the first run carried an approval: %v", ext.submitted)
	}

	approved := func(action string) bool { return action == send }
	skill.Run(RunContext{Ctx: context.Background(), OnLog: func(string) {}, Resume: res.Checkpoint, Approved: approved}, step)
	if got, _ := json.Marshal(ext.submitted["approved"]); string(got) != `["click \"Send\" on mail.google.com"]` {
		t.Fatalf("the rerun sent approved = %s, want only the approved action", got)
	}

	skill.Run(RunContext{Ctx: context.Background(), OnLog: func(string) {}, Resume: res.Checkpoint, Approved: func(string) bool { return false }}, step)
	if _, ok := ext.submitted["approved"]; ok {
		t.Fatalf("a rerun with no yes carried an approval: %v", ext.submitted)
	}
}

// On a desktop with an approval gate a yes is never sent to the extension's server (a token holder could send the
// same), so the step ends asking the person to do it on the screen instead of asking again (round-5 recheck).
func TestBrowserSkillSendsNoApprovalOnAGatedDesktop(t *testing.T) {
	t.Setenv("DECURIX_GATE_REQUIRED", "1")
	send := `click "Send" on mail.google.com`
	ext := &fakeExtServer{status: "done", result: &extResult{Status: "needs_approval",
		Approval: &browserCheckpoint{Action: send, Reason: "it sends"}}}
	server := httptest.NewServer(ext.handler())
	defer server.Close()

	skill := NewBrowser()
	skill.BaseURL = server.URL
	step := Subtask{Step: "reply to Priya", Type: "browser"}
	cp := &browserCheckpoint{Action: send, Reason: "it sends"}
	res, _ := skill.Run(RunContext{Ctx: context.Background(), OnLog: func(string) {}, Resume: cp,
		Approved: func(a string) bool { return a == send }}, step)
	if _, ok := ext.submitted["approved"]; ok {
		t.Fatalf("an approval was sent to the extension on a gated desktop: %v", ext.submitted)
	}
	if res.Verified || res.Meta["needsUserInput"] != true || !strings.Contains(res.Output, "on the screen") {
		t.Fatalf("result = %+v, want the step left to the person on the screen", res)
	}
}

// Only a run the extension finished with an answer is a success. An error, a
// captcha, a stall, a timeout or a "done" with nothing to report is a failure
// the scheduler must not retry - never a verified answer.
func TestBrowserReportVerifiesOnlyAFinishedAnswer(t *testing.T) {
	b := NewBrowser()
	step := Subtask{Step: "look it up", Type: "browser"}
	ok, _ := b.report(step, extRequest{ID: "r1", Status: "done", Result: &extResult{Status: "done", Answer: "42"}})
	if !ok.Verified || ok.Output != "42" {
		t.Fatalf("finished answer = %+v, want verified", ok)
	}
	for _, status := range []string{"error", "timeout", "blocked", "stalled", "paused", "cancelled", "done", "something-new"} {
		res, _ := b.report(step, extRequest{ID: "r1", Status: "done", Result: &extResult{Status: status, Error: "captcha", Log: []string{"opened the page"}}})
		if res.Verified {
			t.Errorf("status %q reported verified: %+v", status, res)
		}
		if res.Meta["fatal"] != true {
			t.Errorf("status %q not marked fatal, the scheduler would retry it", status)
		}
		if !strings.Contains(res.Output, "opened the page") {
			t.Errorf("status %q lost what the browser saw: %q", status, res.Output)
		}
	}
}

// The extension's server refuses a caller without the desktop's agent token: every call carries it, writes as JSON,
// and a refusal is an error, never read as an answer.
func TestBrowserSkillSendsTheAgentToken(t *testing.T) {
	t.Setenv(agentTokenEnv, "tok-123")
	var mu sync.Mutex
	var auths, types []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		if r.Method == http.MethodPost {
			types = append(types, r.Header.Get("Content-Type"))
		}
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer tok-123" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(extRequestsStatus{Requests: []extRequest{{ID: "r1", Status: "done", Result: &extResult{Status: "done", Answer: "ok"}}}})
	}))
	defer server.Close()
	b := NewBrowser()
	b.BaseURL = server.URL
	var status extRequestsStatus
	if err := b.getJSON(context.Background(), "/requests/status", &status); err != nil || len(status.Requests) != 1 {
		t.Fatalf("get = %+v, %v", status, err)
	}
	if err := b.postJSON(context.Background(), "/requests", map[string]any{"task": "x"}, nil); err != nil {
		t.Fatal(err)
	}
	for _, a := range auths {
		if a != "Bearer tok-123" {
			t.Errorf("Authorization = %q", a)
		}
	}
	for _, ct := range types {
		if ct != "application/json" {
			t.Errorf("Content-Type of a write = %q", ct)
		}
	}
	t.Setenv(agentTokenEnv, "")
	if err := b.getJSON(context.Background(), "/requests/status", &status); err == nil {
		t.Error("a 401 was read as an answer")
	}
}

// A browsing task that names an internal address (loopback, metadata, private, localhost) is refused before any
// hand-off: the real browser must not be a way to reach soul's ports, the control plane or the cloud metadata
// service. The extension's server is never even contacted.
func TestBrowserSkillRefusesInternalTargets(t *testing.T) {
	var touched bool
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { touched = true }))
	defer server.Close()
	skill := NewBrowser()
	skill.BaseURL = server.URL

	for _, step := range []string{
		"open http://169.254.169.254/latest/meta-data/ and read it",
		"go to http://localhost:4900/api/chat",
		"fetch http://127.0.0.1:8080/backup",
		"browse http://10.0.0.5/admin",
	} {
		res, err := skill.Run(RunContext{Ctx: context.Background(), OnLog: func(string) {}}, Subtask{Step: step, Type: "browser"})
		if err != nil {
			t.Fatal(err)
		}
		if res.Verified || res.Meta["fatal"] != true {
			t.Errorf("Run(%q) = %+v, want a refusal", step, res)
		}
	}
	if touched {
		t.Error("the extension server was contacted for a refused task")
	}

	// A public target still goes through (it reaches the fake ext server, which has no such request -> handover).
	if res, _ := skill.Run(RunContext{Ctx: context.Background(), OnLog: func(string) {}},
		Subtask{Step: "open https://example.com and search", Type: "browser"}); res.Meta["fatal"] == true && res.Meta["error"] != nil {
		if s, _ := res.Meta["error"].(string); strings.Contains(s, "public internet") {
			t.Errorf("a public target was refused: %+v", res)
		}
	}
}
