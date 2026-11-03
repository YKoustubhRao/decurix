package main

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"strings"
	"time"

	"thebot/internal/skills"
)

// A paused approval is resumed by a reply, and soul's API is reachable by anything in the desktop that holds the
// agent token, the worker's own shell among them. On a desktop with an approval gate (a process of another user that
// only the control plane's signed replies can move, docs/gate-boundary.md in the Decurix repo), a reading of a reply
// sent in the request would be the worker's word, so soul ignores it and asks the gate what became of the person's
// signed reply. A reply with no signed copy resumes the job as "unclear": never as a yes.

// gateDir is where the gate keeps its directory and socket; its existence says this desktop has a gate. A var so a
// test can point it elsewhere.
var gateDir = "/run/decurix-gate"

// gateReplyWait is how long soul waits for the gate to read the person's reply: the control plane's signed copy
// arrives just before the agent's request, but a model reads it.
const gateReplyWait = 45 * time.Second

// gatePresent reports whether this desktop has an approval gate.
func gatePresent() bool {
	st, err := os.Stat(gateDir)
	return err == nil && st.IsDir()
}

// gateReading is the gate's reading of the person's reply: the decision, condition and what they want first. A reply
// the gate never received signed, or a gate that does not answer, is "unclear": it approves nothing.
func gateReading(reply string) *skills.Reading {
	unclear := &skills.Reading{Decision: "unclear"}
	conn, err := net.DialTimeout("unix", gateDir+"/gate.sock", 3*time.Second)
	if err != nil {
		return unclear
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(gateReplyWait + 15*time.Second))
	call, _ := json.Marshal(map[string]any{"op": "reading", "reply": strings.Join(strings.Fields(reply), " "), "wait": int(gateReplyWait.Seconds())})
	if _, err := conn.Write(append(call, '\n')); err != nil {
		return unclear
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return unclear
	}
	var said struct {
		Seen    bool            `json:"seen"`
		Reading *skills.Reading `json:"reading"`
		Error   string          `json:"error"`
	}
	if json.Unmarshal(line, &said) != nil || said.Error != "" || !said.Seen || said.Reading == nil || strings.TrimSpace(said.Reading.Decision) == "" {
		return unclear
	}
	return said.Reading
}
