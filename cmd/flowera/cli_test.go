package main

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// captureStdout runs f with os.Stdout redirected, and returns everything it
// printed. The CLI's command handlers write straight to fmt.Print* rather
// than an injected writer (it is a terminal REPL, not a library), so this is
// what lets a test see what a real invocation would show.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	orig := os.Stdout
	os.Stdout = w

	out := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(r)
		out <- string(data)
	}()

	f()

	w.Close()
	os.Stdout = orig

	return <-out
}

func TestCLIListRendersDesktops(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/desktops" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[{"id":"box","name":"Box","status":"running","profile":"basic","harness":"ollama","model":"qwen2.5-coder:3b"}]`)
	}))
	defer srv.Close()

	client := newAPIClient(srv.URL)

	out := captureStdout(t, func() {
		cliList(context.Background(), client)
	})

	for _, want := range []string{"box", "Box", "running", "basic", "ollama"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q, got:\n%s", want, out)
		}
	}
}

func TestCLICreatePostsFormFields(t *testing.T) {
	var got url.Values

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}

		got = r.Form

		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"box","name":"box","profile":"developer","harness":"claude"}`)
	}))
	defer srv.Close()

	client := newAPIClient(srv.URL)

	captureStdout(t, func() {
		cliCreate(context.Background(), client, []string{
			"box",
			"--profile", "developer",
			"--harness", "claude",
			"--cpus", "4",
			"--memory-mb", "8192",
		})
	})

	want := map[string]string{
		"name":      "box",
		"profile":   "developer",
		"harness":   "claude",
		"cpus":      "4",
		"memory_mb": "8192",
	}

	for field, value := range want {
		if got.Get(field) != value {
			t.Errorf("form field %s = %q, want %q", field, got.Get(field), value)
		}
	}
}

func TestCLIDeleteDeclinedConfirmationDoesNotCallAPI(t *testing.T) {
	called := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"deleted"}`)
	}))
	defer srv.Close()

	client := newAPIClient(srv.URL)
	scanner := bufio.NewScanner(strings.NewReader("no\n"))

	captureStdout(t, func() {
		cliDelete(context.Background(), client, []string{"box"}, scanner)
	})

	if called {
		t.Fatal("delete API was called despite a declined confirmation")
	}
}

func TestCLIDeleteYesFlagSkipsPrompt(t *testing.T) {
	called := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"deleted"}`)
	}))
	defer srv.Close()

	client := newAPIClient(srv.URL)
	scanner := bufio.NewScanner(strings.NewReader(""))

	captureStdout(t, func() {
		cliDelete(context.Background(), client, []string{"box", "--yes"}, scanner)
	})

	if !called {
		t.Fatal("expected the delete API to be called when --yes is given")
	}
}

// TestWaitForReplyStopsOnNewEntry simulates a turn that is still running for
// its first two polls (only the human's own entry exists) and has produced
// the agent's reply by the third - waitForReply should keep polling through
// the first two and stop as soon as the new entry lands.
func TestWaitForReplyStopsOnNewEntry(t *testing.T) {
	var calls int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if atomic.AddInt32(&calls, 1) < 3 {
			io.WriteString(w, `{"desktop_id":"box","entries":[{"id":"1","role":"user","kind":"entry","content":"hi"}],"steps":[{"id":"s1","text":"looking around"}],"running":true,"version":1}`)
			return
		}

		io.WriteString(w, `{"desktop_id":"box","entries":[{"id":"1","role":"user","kind":"entry","content":"hi"},{"id":"2","role":"agent","kind":"entry","content":"hello back"}],"steps":[{"id":"s1","text":"looking around"}],"running":false,"version":2}`)
	}))
	defer srv.Close()

	client := newAPIClient(srv.URL)

	var entriesShown int

	out := captureStdout(t, func() {
		entriesShown, _ = waitForReply(context.Background(), client, "box", 1, 0, 5*time.Millisecond)
	})

	if entriesShown != 2 {
		t.Errorf("entriesShown = %d, want 2", entriesShown)
	}

	if !strings.Contains(out, "hello back") {
		t.Errorf("output missing the agent's reply, got:\n%s", out)
	}

	if !strings.Contains(out, "looking around") {
		t.Errorf("output missing the live step printed while running, got:\n%s", out)
	}
}

func TestCLIPauseAndUnpauseAction(t *testing.T) {
	var calledPaths []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calledPaths = append(calledPaths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"ok"}`)
	}))
	defer srv.Close()

	client := newAPIClient(srv.URL)

	outPause := captureStdout(t, func() {
		cliSimpleAction(context.Background(), client, "/api/desktops/pause", []string{"box"}, "paused")
	})
	if !strings.Contains(outPause, "box paused") {
		t.Errorf("expected 'box paused', got %q", outPause)
	}

	outUnpause := captureStdout(t, func() {
		cliSimpleAction(context.Background(), client, "/api/desktops/unpause", []string{"box"}, "woken")
	})
	if !strings.Contains(outUnpause, "box woken") {
		t.Errorf("expected 'box woken', got %q", outUnpause)
	}

	if len(calledPaths) != 2 || calledPaths[0] != "/api/desktops/pause" || calledPaths[1] != "/api/desktops/unpause" {
		t.Errorf("unexpected called paths: %v", calledPaths)
	}
}
