package main

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"strings"
	"time"

	"thebot/internal/scheduler"
	"thebot/internal/skills"
)

// A paused approval is resumed by a reply, and soul's API is reachable by anything in the desktop that holds the
// agent token, the worker's own shell among them. On a desktop with an approval gate (a process of another user that
// only the control plane's signed replies can move, docs/gate-boundary.md in the Decurix repo), a reading of a reply
// sent in the request would be the worker's word, so soul ignores it.
//
// Instead a paused approval is a request of the gate's own (gateAsk): the gate words the question the person sees and
// gives the request an id, which soul keeps in the checkpoint. The reply that resumes the job is the gate's
// settlement of that one request (gateSettled), taken once. A reply the gate did not settle for this request - no
// signed copy, an answer to another question, the worker's own "yes" - resumes it as "unclear": never as a yes.

// gateDir is where the gate keeps its directory and socket; its existence says this desktop has a gate. A var so a
// test can point it elsewhere.
var gateDir = "/run/decurix-gate"

// gateReplyWait is how long soul waits for the gate to settle the person's reply: the control plane's signed copy
// arrives just before the agent's request, but a model reads it. Waited for in the job, never in the HTTP call.
const gateReplyWait = 45 * time.Second

// gateRequiredEnv is set to "1" by the control plane on every desktop it makes. It can only turn the gate on.
const gateRequiredEnv = "DECURIX_GATE_REQUIRED"

// gatePresent reports whether this desktop has, or must have, an approval gate: its directory exists, or the control
// plane said so. A gate that is down then settles nothing, so no reply resumes a paused approval as a yes; soul never
// falls back to believing a reading sent in the request.
func gatePresent() bool {
	if os.Getenv(gateRequiredEnv) == "1" {
		return true
	}
	st, err := os.Stat(gateDir)
	return err == nil && st.IsDir()
}

// gateCall sends one call to the gate and decodes its answer into out; false when the gate cannot be reached or
// answers an error.
func gateCall(call map[string]any, timeout time.Duration, out any) bool {
	conn, err := net.DialTimeout("unix", gateDir+"/gate.sock", 3*time.Second)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	raw, _ := json.Marshal(call)
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		return false
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return false
	}
	var failed struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(line, &failed) != nil || failed.Error != "" {
		return false
	}
	return json.Unmarshal(line, out) == nil
}

// gateAsk makes a paused approval a request of the gate's: cp gets the gate's request id and fingerprint. A gate that
// does not answer leaves them empty, and the reply that resumes the job is then never a yes.
func gateAsk(cp *scheduler.Checkpoint) {
	command, request, ok := cp.Approval()
	if !ok {
		return
	}
	var said struct {
		Request *struct {
			ID          string `json:"id"`
			Fingerprint string `json:"fingerprint"`
		} `json:"request"`
	}
	call := map[string]any{"op": "check", "reason": "it needs your approval",
		"material": map[string]any{"kind": "command", "command": command, "request": request, "workdir": cp.WorkDir}}
	if gateCall(call, 15*time.Second, &said) && said.Request != nil {
		cp.GateRequest, cp.GateFingerprint = said.Request.ID, said.Request.Fingerprint
	}
}

// gateSettled is the gate's reading of the person's reply to cp's own request: the decision, condition and what they
// want first. Anything the gate did not settle for that request is "unclear": it approves nothing.
func gateSettled(cp *scheduler.Checkpoint) *skills.Reading {
	unclear := &skills.Reading{Decision: "unclear"}
	if cp == nil || cp.GateRequest == "" {
		return unclear
	}
	var said struct {
		Seen    bool            `json:"seen"`
		Outcome *string         `json:"outcome"`
		Reading *skills.Reading `json:"reading"`
	}
	call := map[string]any{"op": "settled", "request_id": cp.GateRequest, "fingerprint": cp.GateFingerprint,
		"wait": int(gateReplyWait.Seconds())}
	if !gateCall(call, gateReplyWait+15*time.Second, &said) || !said.Seen || said.Reading == nil ||
		strings.TrimSpace(said.Reading.Decision) == "" {
		return unclear
	}
	// The gate's own outcome decides a yes: a reading of "yes" that made no grant is not one.
	if strings.EqualFold(strings.TrimSpace(said.Reading.Decision), "yes") && (said.Outcome == nil || *said.Outcome != "granted") {
		return unclear
	}
	return said.Reading
}
