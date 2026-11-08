package main

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"thebot/internal/scheduler"
)

// fakeGate answers every call with line, on the socket a gate would listen on, and returns the calls it got.
func fakeGate(t *testing.T, line string) func() []map[string]any {
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
				_, _ = c.Write([]byte(line + "\n"))
			}()
		}
	}()
	return func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), calls...)
	}
}

func pausedOn(command string) *scheduler.Checkpoint {
	return &scheduler.Checkpoint{Kind: "approval", Action: command, WorkDir: "/work", GateRequest: "req-1", GateFingerprint: "fp-1"}
}

func TestGateAskMakesThePausedCommandTheGatesRequest(t *testing.T) {
	calls := fakeGate(t, `{"allowed": false, "request": {"id": "req-9", "fingerprint": "fp-9", "question": "Approval needed"}}`)
	cp := &scheduler.Checkpoint{Kind: "approval", Action: "rm -rf build", WorkDir: "/work"}
	gateAsk(cp)
	if cp.GateRequest != "req-9" || cp.GateFingerprint != "fp-9" {
		t.Fatalf("request %q fingerprint %q", cp.GateRequest, cp.GateFingerprint)
	}
	got := calls()
	material, _ := got[0]["material"].(map[string]any)
	if got[0]["op"] != "check" || material["kind"] != "command" || material["command"] != "rm -rf build" {
		t.Fatalf("call %v", got[0])
	}
}

func TestGateSettledIsTheGatesSettlementOfThisRequest(t *testing.T) {
	calls := fakeGate(t, `{"seen": true, "outcome": "granted", "reading": {"decision": "yes", "condition": ""}}`)
	if r := gateSettled(pausedOn("rm -rf build")); r.Decision != "yes" {
		t.Fatalf("a settled yes read as %q", r.Decision)
	}
	if got := calls(); got[0]["op"] != "settled" || got[0]["request_id"] != "req-1" {
		t.Fatalf("asked the gate %v", got[0])
	}
}

func TestAYesTheGateMadeNoGrantForApprovesNothing(t *testing.T) {
	for _, line := range []string{
		`{"seen": true, "outcome": null, "reading": {"decision": "yes"}}`,
		`{"seen": true, "outcome": "declined", "reading": {"decision": "yes"}}`,
		`{"seen": false, "outcome": null, "reading": null}`,
	} {
		fakeGate(t, line)
		if r := gateSettled(pausedOn("rm -rf build")); r.Decision != "unclear" {
			t.Errorf("%s read as %q", line, r.Decision)
		}
	}
}

func TestAPausedJobWithoutAGateRequestApprovesNothing(t *testing.T) {
	calls := fakeGate(t, `{"seen": true, "outcome": "granted", "reading": {"decision": "yes"}}`)
	cp := pausedOn("rm -rf build")
	cp.GateRequest = ""
	if r := gateSettled(cp); r.Decision != "unclear" {
		t.Fatalf("read as %q", r.Decision)
	}
	if len(calls()) != 0 {
		t.Fatal("asked the gate about no request")
	}
}

func TestAGateThatAnswersNonsenseOrNothingApprovesNothing(t *testing.T) {
	for _, line := range []string{`not json`, `{"error": "OSError"}`, `{"seen": true, "outcome": "granted", "reading": {"decision": ""}}`, `{"seen": true}`} {
		fakeGate(t, line)
		if r := gateSettled(pausedOn("x")); r.Decision != "unclear" {
			t.Errorf("%s read as %q", line, r.Decision)
		}
	}
	old := gateDir
	gateDir = filepath.Join(os.TempDir(), "no-such-gate-dir")
	defer func() { gateDir = old }()
	if gatePresent() {
		t.Fatal("a missing directory is no gate")
	}
	if r := gateSettled(pausedOn("x")); r.Decision != "unclear" {
		t.Fatalf("an unreachable gate read as %q", r.Decision)
	}
}

// TestADesktopThatMustHaveAGateNeverBelievesTheRequest checks the control plane's flag makes soul treat a missing gate
// as one that settles nothing (round-4 recheck: no gate directory meant the request's own reading was believed).
func TestADesktopThatMustHaveAGateNeverBelievesTheRequest(t *testing.T) {
	old := gateDir
	gateDir = filepath.Join(os.TempDir(), "no-such-gate-dir")
	defer func() { gateDir = old }()
	t.Setenv(gateRequiredEnv, "1")
	if !gatePresent() {
		t.Fatal("a desktop that must have a gate was taken for one without")
	}
	if r := gateSettled(pausedOn("x")); r.Decision != "unclear" {
		t.Fatalf("a missing gate read as %q", r.Decision)
	}
	t.Setenv(gateRequiredEnv, "0")
	if gatePresent() {
		t.Fatal("the flag can only turn the gate on")
	}
}

// A gui command the gate stopped on the screen is asked about once, as the gate's own request for that action: found
// live (2026-11-08), a second request of soul's for the command replaced the action's, the yes granted the command, and
// the gui command run on it was asked about again.
func TestGateAskAdoptsTheGatesRequestForAnActionItStoppedOnTheScreen(t *testing.T) {
	calls := fakeGate(t, `{"pending": {"id": "req-act", "fingerprint": "fp-act", "question": "Approval needed before I click"}}`)
	cp := &scheduler.Checkpoint{Kind: "approval", Action: "gui click e6 --checked", WorkDir: "/work", OnScreen: true}
	if gateAsk(cp) {
		t.Fatal("an adopted request is no refusal")
	}
	if cp.GateRequest != "req-act" || cp.GateFingerprint != "" {
		t.Fatalf("request %q fingerprint %q: the action's request, its grant left for the gui command", cp.GateRequest, cp.GateFingerprint)
	}
	got := calls()
	if len(got) != 1 || got[0]["op"] != "pending" {
		t.Fatalf("calls %v: no second request of soul's may replace the action's", got)
	}
}

func TestGateAskAsksAboutTheCommandWhenTheGateHoldsNoScreenRequest(t *testing.T) {
	calls := fakeGate(t, `{"pending": null, "allowed": false, "request": {"id": "req-9", "fingerprint": "fp-9", "question": "Approval needed"}}`)
	cp := &scheduler.Checkpoint{Kind: "approval", Action: "gui click e6", WorkDir: "/work", OnScreen: true}
	gateAsk(cp)
	if cp.GateRequest != "req-9" || cp.GateFingerprint != "fp-9" {
		t.Fatalf("request %q fingerprint %q", cp.GateRequest, cp.GateFingerprint)
	}
	if got := calls(); len(got) != 2 || got[0]["op"] != "pending" || got[1]["op"] != "check" {
		t.Fatalf("calls %v", got)
	}
}

func TestGateAskNeverAdoptsForACommandTheGuardStopped(t *testing.T) {
	calls := fakeGate(t, `{"pending": {"id": "req-act", "fingerprint": "fp-act"}, "allowed": false, "request": {"id": "req-9", "fingerprint": "fp-9"}}`)
	cp := &scheduler.Checkpoint{Kind: "approval", Action: "rm -rf build", WorkDir: "/work"}
	gateAsk(cp)
	if cp.GateRequest != "req-9" {
		t.Fatalf("a guard's pause took the gate's pending screen request %q", cp.GateRequest)
	}
	if got := calls(); len(got) != 1 || got[0]["op"] != "check" {
		t.Fatalf("calls %v", got)
	}
}
