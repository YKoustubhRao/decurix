// Tests for paused jobs kept across a restart (paused.go) and the job stream's heartbeat: a paused job survives a new
// soul over the same folder; one with secret values is never written and never resumed; a file changed, moved or too
// old is dropped; a restored approval is resumed only by the gate's settlement of its own request.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"thebot/internal/config"
	"thebot/internal/llm"
	"thebot/internal/memory"
	"thebot/internal/scheduler"
	"thebot/internal/withheld"
	"thebot/internal/workspace"
)

// restarted is a new soul over s's folder with s's agent token: what a restart of the process or its container gives.
func restarted(t *testing.T, s *server) *server {
	t.Helper()
	ws, err := workspace.New(s.ws.Root())
	if err != nil {
		t.Fatal(err)
	}
	l2, err := memory.NewL2(ws.Root())
	if err != nil {
		t.Fatal(err)
	}
	sched := scheduler.New(stubProvider{}, llm.DefaultModels("stub-model"), ws, l2, nil, nil, nil)
	return newServer(config.Config{OllamaHost: "stub", AgentToken: s.cfg.AgentToken}, stubProvider{}, ws, sched)
}

// pausePush starts a push in session and waits for it to pause for approval.
func pausePush(t *testing.T, s *server, session, command string, secrets ...string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"prompt": "run this command: `" + command + "`", "sessionId": session, "secrets": secrets})
	job := waitJob(t, s, postChat(t, s, string(body)), 10*time.Second)
	if job["outcome"] != "needs_approval" {
		t.Fatalf("job = %v, want a paused approval", job)
	}
}

// pausedBody is the signed body of session's paused file.
func pausedBody(t *testing.T, s *server, session string) (raw []byte, body pausedFile) {
	t.Helper()
	raw, err := os.ReadFile(s.pausedDisk.path(session))
	if err != nil {
		t.Fatalf("no paused file: %v", err)
	}
	_, rest, _ := bytes.Cut(raw, []byte("\n"))
	if err := json.Unmarshal(rest, &body); err != nil {
		t.Fatal(err)
	}
	return raw, body
}

// A paused approval kept on disk is found by the reply after a restart, and the approved command runs once.
func TestAPausedJobSurvivesARestart(t *testing.T) {
	fakeGit(t)
	s := newTestServer(t)
	pausePush(t, s, "desk", "git push origin main")
	path := s.pausedDisk.path("desk")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the paused job was not kept on disk: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("paused file mode %v, want 0600", info.Mode().Perm())
	}
	if !strings.HasPrefix(filepath.ToSlash(path), filepath.ToSlash(filepath.Join(s.ws.Root(), ".memory", "paused"))) {
		t.Errorf("paused file at %s, want it under .memory (outside what the Data RAG ingests)", path)
	}

	again := restarted(t, s)
	resumed := waitJob(t, again, postChat(t, again, `{"prompt":"yes","sessionId":"desk","reply":true}`), 10*time.Second)
	if resumed["resumed"] != true || !strings.Contains(resumed["finalAnswer"].(string), "pushed-for-real") {
		t.Fatalf("after a restart the reply = %v, want the paused push resumed and run", resumed)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the taken job's file is still there (%v)", err)
	}
	third := restarted(t, again)
	if len(third.paused) != 0 {
		t.Fatalf("a taken job came back after another restart: %v", third.paused)
	}
}

// A paused job with secret values writes none of them (nothing of the job at all, only a marker), and after a restart
// it is dropped: the reply finds nothing waiting and runs nothing of the paused job.
func TestAPausedJobWithSecretsIsNeverWrittenNorResumedAfterARestart(t *testing.T) {
	fakeGit(t)
	s := newTestServer(t)
	pausePush(t, s, "desk", "git push https://example.com/"+testSecret+".git main", testSecret)

	raw, body := pausedBody(t, s, "desk")
	for _, form := range append(withheld.WrittenForms(testSecret), testSecret, "git push") {
		if bytes.Contains(raw, []byte(form)) {
			t.Fatalf("the paused file holds %q:\n%s", form, raw)
		}
	}
	if !body.HadSecrets || len(body.Checkpoint) != 0 {
		t.Fatalf("paused file body = %+v, want only a marker that it had secrets", body)
	}

	again := restarted(t, s)
	if len(again.paused) != 0 {
		t.Fatal("a paused job with secrets was restored")
	}
	if _, err := os.Stat(s.pausedDisk.path("desk")); !os.IsNotExist(err) {
		t.Fatalf("the marker was not removed after it was refused (%v)", err)
	}
	next := waitJob(t, again, postChat(t, again, `{"prompt":"yes","sessionId":"desk","reply":true}`), 10*time.Second)
	if next["resumed"] == true || strings.Contains(next["finalAnswer"].(string), "pushed-for-real") {
		t.Fatalf("the reply = %v, want nothing waiting", next)
	}
}

// Text that looks like a secret by its shape (what soul's secrets filter withholds) is never written either, even when
// nobody marked it.
func TestAPausedJobWithASecretShapedValueIsOnlyAMarker(t *testing.T) {
	fakeGit(t)
	s := newTestServer(t)
	token := "ghp_" + strings.Repeat("Ab1", 10)
	pausePush(t, s, "desk", "git push https://"+token+"@example.com/r.git main")
	raw, body := pausedBody(t, s, "desk")
	if bytes.Contains(raw, []byte(token)) || !body.HadSecrets {
		t.Fatalf("paused file = %s", raw)
	}
}

// A paused file changed by anyone but soul (the worker's own commands can write its folder), made up, moved to another
// session's name or past the wait is dropped at startup.
func TestAPausedFileThatWasChangedMovedOrIsTooOldIsDropped(t *testing.T) {
	fakeGit(t)
	s := newTestServer(t)
	pausePush(t, s, "desk", "git push origin main")
	pausePush(t, s, "other", "git push origin main")
	pausePush(t, s, "old", "git push origin main")

	// desk: the command it waits on, changed.
	path := s.pausedDisk.path("desk")
	raw, _ := os.ReadFile(path)
	changed := bytes.ReplaceAll(raw, []byte("git push origin main"), []byte("git push origin evil"))
	if bytes.Equal(changed, raw) {
		t.Fatal("test setup: nothing to change")
	}
	if err := os.WriteFile(path, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	// other: moved to a session of its own name.
	moved := s.pausedDisk.path("stolen")
	otherRaw, _ := os.ReadFile(s.pausedDisk.path("other"))
	if err := os.WriteFile(moved, otherRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	// old: signed by soul, but past the wait.
	s.pausedMu.Lock()
	old := s.paused["old"]
	s.pausedMu.Unlock()
	old.CreatedAt = time.Now().Add(-pausedJobTTL - time.Hour)
	s.pausedDisk.save("old", old, nil)
	// made up: signed with another desktop's key.
	forger := newPausedStore(s.ws.Root(), "another-desktops-token")
	forger.save("forged", &scheduler.Checkpoint{Kind: "approval", Action: "git push origin main", CreatedAt: time.Now()}, nil)

	again := restarted(t, s)
	if len(again.paused) != 1 || again.paused["other"] == nil {
		t.Fatalf("restored %v, want only the untouched one", keysOf(again.paused))
	}
	for _, gone := range []string{"desk", "stolen", "old", "forged"} {
		if _, err := os.Stat(s.pausedDisk.path(gone)); !os.IsNotExist(err) {
			t.Errorf("the refused file of %q is still there (%v)", gone, err)
		}
	}
	next := waitJob(t, again, postChat(t, again, `{"prompt":"yes","sessionId":"desk","reply":true}`), 10*time.Second)
	if next["resumed"] == true {
		t.Fatalf("a changed paused file was resumed: %v", next)
	}
}

// keysOf is m's sessions.
func keysOf(m map[string]*scheduler.Checkpoint) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Without an agent token nothing is kept on disk: there is no key to sign it with.
func TestNoPausedJobIsKeptWithoutAnAgentToken(t *testing.T) {
	if newPausedStore(t.TempDir(), "") != nil {
		t.Fatal("a store without a key")
	}
	var none *pausedStore
	none.save("desk", &scheduler.Checkpoint{CreatedAt: time.Now()}, nil)
	none.remove("desk")
	if got := none.load(pausedJobTTL); len(got) != 0 {
		t.Fatal("a nil store loaded something")
	}
}

// scriptedGate answers each call by its op, on the socket a gate listens on, and returns the calls it got.
func scriptedGate(t *testing.T, answer func(op string) string) func() []map[string]any {
	t.Helper()
	dir, err := os.MkdirTemp("", "gate")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ln, err := net.Listen("unix", filepath.Join(dir, "gate.sock"))
	if err != nil {
		t.Skipf("no unix sockets here: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	old := gateDir
	gateDir = dir
	t.Cleanup(func() { gateDir = old })
	var mu sync.Mutex
	var calls []map[string]any
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				raw, _ := bufio.NewReader(c).ReadBytes('\n')
				var call map[string]any
				_ = json.Unmarshal(raw, &call)
				mu.Lock()
				calls = append(calls, call)
				mu.Unlock()
				op, _ := call["op"].(string)
				_, _ = c.Write([]byte(answer(op) + "\n"))
			}()
		}
	}()
	return func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), calls...)
	}
}

// A restored approval is resumed only by the gate's settlement of the request it was paused with: a gate that no
// longer knows that request (it restarted too) settles nothing, and the person's "yes" in the request body approves
// nothing; a gate that settled it with a grant runs the command, asked by the same request id and fingerprint.
func TestARestoredApprovalIsResumedOnlyByTheGatesSettlementOfItsRequest(t *testing.T) {
	for _, tc := range []struct {
		name    string
		settled string
		runs    bool
	}{
		{"the gate lost the request", `{"seen": false, "outcome": null, "reading": null}`, false},
		{"the gate is down", `not json`, false},
		{"the gate settled it with a grant", `{"seen": true, "outcome": "granted", "reading": {"decision": "yes"}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeGit(t)
			calls := scriptedGate(t, func(op string) string {
				if op == "check" {
					return `{"allowed": false, "request": {"id": "req-7", "fingerprint": "fp-7"}}`
				}
				return tc.settled
			})
			s := newTestServer(t)
			pausePush(t, s, "desk", "git push origin main")
			_, body := pausedBody(t, s, "desk")
			if !bytes.Contains(body.Checkpoint, []byte(`"gateRequest":"req-7"`)) {
				t.Fatalf("the kept checkpoint lost its gate request: %s", body.Checkpoint)
			}

			again := restarted(t, s)
			reply := `{"prompt":"yes","sessionId":"desk","reply":true,"approvalReading":{"decision":"yes"}}`
			next := waitJob(t, again, postChat(t, again, reply), 70*time.Second)
			ran := strings.Contains(next["finalAnswer"].(string), "pushed-for-real")
			if ran != tc.runs || next["resumed"] != true {
				t.Fatalf("resumed %v, ran %v (want %v): %v", next["resumed"], ran, tc.runs, next)
			}
			var settled map[string]any
			for _, c := range calls() {
				if c["op"] == "settled" {
					settled = c
				}
			}
			if settled == nil || settled["request_id"] != "req-7" || settled["fingerprint"] != "fp-7" {
				t.Fatalf("the gate was asked %v, want the settlement of req-7/fp-7", settled)
			}
		})
	}
}

// An open job stream gets a ": ping" comment line every sseHeartbeat while the job runs, then its final event.
func TestTheJobStreamSendsAHeartbeat(t *testing.T) {
	old := sseHeartbeat
	sseHeartbeat = 30 * time.Millisecond
	defer func() { sseHeartbeat = old }()
	s := newTestServer(t)
	job := newJobRecord("beat", "a long job")
	s.jobsMu.Lock()
	s.jobs[job.ID] = job
	s.jobsMu.Unlock()

	srv := httptest.NewServer(s.mux())
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/jobs/beat/stream", nil)
	req.Header.Set("Authorization", "Bearer "+testAgentToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	pings := 0
	timeout := time.After(5 * time.Second)
	for pings < 2 {
		select {
		case l := <-lines:
			if l == ": ping" {
				pings++
			}
		case <-timeout:
			t.Fatalf("%d heartbeat(s) in 5s, want a ping every %s", pings, sseHeartbeat)
		}
	}
	job.finish()
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatal("the stream ended without its final event")
			}
			if strings.HasPrefix(l, "data: ") && strings.Contains(l, `"type":"final"`) {
				return
			}
		case <-timeout:
			t.Fatal("no final event after the job finished")
		}
	}
}

// GET /api/paused says, without taking it, whether a session's paused job was read back after a restart: the agent
// asks it before closing a waiting task as lost.
func TestThePausedRouteSaysWhatARestartKept(t *testing.T) {
	fakeGit(t)
	s := newTestServer(t)
	pausePush(t, s, "desk", "git push origin main")
	again := restarted(t, s)

	ask := func(session string) map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		again.handlePaused(rec, httptest.NewRequest(http.MethodGet, "/api/paused?session="+session, nil))
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := ask("desk"); got["paused"] != true || got["kind"] != "approval" {
		t.Fatalf("paused = %v, want the kept approval", got)
	}
	if got := ask("desk"); got["paused"] != true {
		t.Fatal("asking took the paused job")
	}
	if got := ask("other"); got["paused"] != false {
		t.Fatalf("another session = %v, want none", got)
	}
}
