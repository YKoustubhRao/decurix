package main

import (
	"bytes"
	"encoding/json"
	"net/http"

	"thebot/internal/escalation"
)

// /api/llm/escalation is the escalation ladder's model questions made by soul (plan rule 3, phase 2: the second
// decision moved, py.escalation.*). When a taught skill's replay cannot find a control or its recorded result, the
// ladder (agent/decurix_agent/computer/escalation.py) asks a model one of four questions - ground, judge, detour,
// point - named by the call's purpose (py.escalation.<kind>). Here the agent sends only the evidence, as data (the
// candidates, the screen as its tools render it, the lines of text the recording's frames held); soul words the prompt
// (escalation.Prepare, byte for byte today's), makes the call exactly as /api/llm/call does (makeCall: the same
// provider chain, brain, purpose, tags, trace and limits) and reads the answer (Question.Decide), answering {text,
// truncated, usage, model, provider, decision}.
//
// Behind DECURIX_SOUL_ESCALATION on the agent's side, off by default: until a 5-repeat eval shows the switch changes
// nothing, today's path (escalation.py's own prompts and readers) stays, and agent/tests/test_escalation_parity.py with
// internal/escalation's parity test hold the two to one recorded corpus.
//
// A decision is what the ladder acts on: a Pick (ground, point), a Judgement (judge) or a Skip (detour). The agent
// checks its shape and takes the step the same way as for its own reading; what it does with it - asking a person,
// leaving steps out, the trail - stays the agent's. Either brain may be asked (the call's brain, as /api/llm/call): the
// ladder asks the hosted model first and a desktop's local one otherwise (harness.ask_decided). The approval gate never
// calls here (as /api/llm/call: soul_call refuses to under IN_GATE), and no rung of the ladder can grant an approval.
//
// The agent token opens it, never soul's call key (guard.go): the key is for the `gui` commands' own calls, and their
// ladders ask no model; a process with only the key is refused (401) and takes today's path.
//
// llmEscalationPath is the route.
const llmEscalationPath = "/api/llm/escalation"

// escalationCallRequest is a question of the ladder: a call's fields (brain, purpose, task, turn, limits, secrets) with
// the evidence in place of messages.
type escalationCallRequest struct {
	llmCallRequest
	Evidence json.RawMessage `json:"evidence"`
}

// handleLLMEscalation asks the ladder's question from its evidence and answers the call with soul's decision.
func (s *server) handleLLMEscalation(w http.ResponseWriter, r *http.Request) {
	var req escalationCallRequest
	if !decodeCall(w, r, &req) {
		return
	}
	refuse := func(why string) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": why, "where": "soul"})
	}
	kind, ok := escalation.KindOf(req.Purpose)
	switch {
	case len(bytes.TrimSpace(req.Evidence)) == 0 || string(bytes.TrimSpace(req.Evidence)) == "null":
		refuse("a question of the ladder needs its evidence")
		return
	case len(req.Messages) > 0:
		refuse("a question of the ladder is asked from evidence, not messages")
		return
	case !ok:
		refuse("a question of the ladder's purpose is py.escalation.ground, .judge, .detour or .point")
		return
	}
	question, err := escalation.Prepare(kind, req.Evidence)
	if err != nil {
		refuse("the evidence is not a " + kind + " question's")
		return
	}
	call := req.llmCallRequest
	for _, m := range question.Messages() {
		content, err := json.Marshal(m.Content)
		if err != nil {
			refuse("the evidence could not be worded")
			return
		}
		call.Messages = append(call.Messages, callMessage{Role: m.Role, Content: content})
	}
	s.makeCall(w, r, call, question.Decide)
}
