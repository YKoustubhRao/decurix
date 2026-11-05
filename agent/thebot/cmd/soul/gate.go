package main

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"regexp"
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
//
// refused reports that the gate answered and would not ask at all (handoff H-SOUL-8): it could not word the action's
// question without the task's secrets, so no person can be asked and no reply could ever approve it. The gate's
// answer is then {"allowed": false, "refused": true, "request": null}; an answer with allowed false and no request,
// from a gate that predates the field, is read the same way. The caller treats it as a no to that one action.
func gateAsk(cp *scheduler.Checkpoint) (refused bool) {
	command, request, ok := cp.Approval()
	if !ok {
		return false
	}
	var said struct {
		Allowed bool `json:"allowed"`
		Refused bool `json:"refused"`
		Request *struct {
			ID          string `json:"id"`
			Fingerprint string `json:"fingerprint"`
		} `json:"request"`
	}
	material := map[string]any{"kind": "command", "command": command, "request": request, "workdir": cp.WorkDir}
	if target := commandTarget(command); target != nil {
		material["target"] = target // what the ref is, so the gate's question names it (H-PY-17); not fingerprinted
	}
	call := map[string]any{"op": "check", "reason": "it needs your approval", "material": material}
	if !gateCall(call, 15*time.Second, &said) {
		return false // no answer: the job waits, and no reply is ever a yes
	}
	if said.Refused || (!said.Allowed && said.Request == nil) {
		return true
	}
	if said.Request != nil {
		cp.GateRequest, cp.GateFingerprint = said.Request.ID, said.Request.Fingerprint
	}
	return false
}

// gateRefusalNote is what the operator is told when the gate refused to ask about its action: a no to that one
// action, with why, so the worker can do the work another way or report what it could not do.
const gateRefusalNote = "The action was not allowed and did not run: its approval question could not be shown to " +
	"the person safely (the approval gate could not word it without the task's secrets), so nobody could be asked. " +
	"Do not try the same action again. Do the work another way that needs no such approval, or report what you " +
	"could not do and why."

// maxGateRefusals bounds how many of a job's actions in a row the gate may refuse before the job ends.
const maxGateRefusals = 3

// guiRefCommandRe is a gui command on an element ref ("gui click e33", "gui type --enter e7 x"): the agent's
// approval._GUI_REF_COMMAND, the same pattern.
var guiRefCommandRe = regexp.MustCompile(`(?i)^\s*gui\s+([a-z][a-z_-]*)\s+(?:--?[\w-]+(?:=\S+)?\s+)*(e\d+)\b`)

// observationFile is the latest observation the gui command saved (the agent's computer/core.py STATE_FILE): what a
// ref means on the screen now. A var so a test can point it elsewhere.
var observationFile = func() string {
	dir := os.Getenv("DECURIX_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".decurix")
	}
	return filepath.Join(dir, "computer", "observation.json")
}

// commandTarget is what the ref of a held gui command acts on, as the agent's approval.command_target gives it (H-PY-17):
// {"ref", "role", "name", "window_title"} from the element of the latest observation with that ref, the window title
// being the element's own, else its group's, else the observation's active window; {"ref", "unresolved": true} when
// the observation has no such element or cannot be read; nil for a command that names no ref.
func commandTarget(command string) map[string]any {
	m := guiRefCommandRe.FindStringSubmatch(command)
	if m == nil {
		return nil
	}
	ref := m[2]
	unresolved := map[string]any{"ref": ref, "unresolved": true}
	raw, err := os.ReadFile(observationFile())
	if err != nil {
		return unresolved
	}
	var obs struct {
		ActiveTitle string           `json:"active_title"`
		Groups      []map[string]any `json:"groups"`
		Elements    []map[string]any `json:"elements"`
	}
	if json.Unmarshal(raw, &obs) != nil {
		return unresolved
	}
	text := func(v any) string {
		if s, ok := v.(string); ok {
			return s
		}
		return ""
	}
	key := func(v any) string {
		if v == nil {
			return ""
		}
		b, _ := json.Marshal(v)
		return string(b)
	}
	titles := map[string]string{}
	for _, g := range obs.Groups {
		title := text(g["window_title"])
		if title == "" {
			title = text(g["title"])
		}
		titles[key(g["id"])] = title
	}
	for _, e := range obs.Elements {
		if text(e["ref"]) != ref {
			continue
		}
		window := text(e["window_title"])
		if window == "" && e["group"] != nil {
			window = titles[key(e["group"])]
		}
		if window == "" {
			window = obs.ActiveTitle
		}
		return map[string]any{"ref": ref, "role": text(e["role"]), "name": text(e["name"]), "window_title": window}
	}
	return unresolved
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
