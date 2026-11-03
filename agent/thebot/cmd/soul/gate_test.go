package main

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// fakeGate answers every call with line, on the socket a gate would listen on.
func fakeGate(t *testing.T, line string) {
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
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = bufio.NewReader(c).ReadBytes('\n')
				_, _ = c.Write([]byte(line + "\n"))
			}()
		}
	}()
}

func TestGateReadingIsTheGatesNotTheRequests(t *testing.T) {
	fakeGate(t, `{"seen": true, "reading": {"decision": "yes", "condition": ""}}`)
	if r := gateReading("yes"); r.Decision != "yes" {
		t.Fatalf("a signed yes read as %q", r.Decision)
	}
}

func TestAReplyTheGateNeverReceivedSignedApprovesNothing(t *testing.T) {
	fakeGate(t, `{"seen": false, "reading": null}`)
	if r := gateReading("yes"); r.Decision != "unclear" {
		t.Fatalf("an unsigned reply read as %q", r.Decision)
	}
}

func TestAGateThatAnswersNonsenseOrNothingApprovesNothing(t *testing.T) {
	for _, line := range []string{`not json`, `{"error": "OSError"}`, `{"seen": true, "reading": {"decision": ""}}`, `{"seen": true}`} {
		fakeGate(t, line)
		if r := gateReading("yes"); r.Decision != "unclear" {
			t.Errorf("%s read as %q", line, r.Decision)
		}
	}
	old := gateDir
	gateDir = filepath.Join(os.TempDir(), "no-such-gate-dir")
	defer func() { gateDir = old }()
	if gatePresent() {
		t.Fatal("a missing directory is no gate")
	}
	if r := gateReading("yes"); r.Decision != "unclear" {
		t.Fatalf("an unreachable gate read as %q", r.Decision)
	}
}
