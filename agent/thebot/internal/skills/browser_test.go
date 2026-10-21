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
