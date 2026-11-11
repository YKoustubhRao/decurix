package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"thebot/internal/judge"
)

// /api/llm/judge is the final check made by soul (plan rule 3, phase 2: the first decision moved, py.judge). Phase 1
// moved the transport - the agent's messages through soul's provider (llmcall.go). Here the agent sends only the
// evidence, as data (verify.judge_evidence: the task, the values, what the tools observed, the files' facts, the
// pictures as data: URLs); soul words the prompt (judge.Messages, byte for byte today's), makes the call exactly as
// /api/llm/call does (makeCall: the same provider chain, purpose, tags, trace and limits) and reads the answer
// (judge.Decide), answering {text, truncated, usage, model, provider, decision}.
//
// Behind DECURIX_SOUL_JUDGE on the agent's side, off by default: until a 5-repeat eval shows the switch changes
// nothing, today's path (verify.py's own prompt and _parse) stays, and agent/tests/test_judge_parity.py with
// internal/judge's parity test hold the two to one recorded corpus.
//
// The error mapping stays the agent's: a decision is a verdict or {"error", "cut_off"} for an answer that could not be
// read, which verify.judge maps to unsure as it maps its own _parse's errors. The approval gate never calls here (as
// /api/llm/call: soul_call refuses to under IN_GATE).
//
// The agent token opens it, never soul's call key (guard.go): the key is for the `gui` commands' own calls, and none
// of them judges; a process with only the key is refused (401) and takes today's path.
//
// llmJudgePath is the route.
const llmJudgePath = "/api/llm/judge"

// judgeCallRequest is a call to the judge: a call's fields (brain, purpose, task, turn, limits, secrets) with the
// evidence in place of messages, and for a second judgement the terms the judge asked to see and the lines found.
type judgeCallRequest struct {
	llmCallRequest
	Evidence *judge.Evidence `json:"evidence"`
	Look     []string        `json:"look"`
	Closer   string          `json:"closer"`
}

// judgePurpose is whether purpose is the final check's: py.judge, or one of its kinds (py.judge.spot).
func judgePurpose(purpose string) bool {
	return purpose == "py.judge" || strings.HasPrefix(purpose, "py.judge.")
}

// handleLLMJudge makes the final check from its evidence and answers the call with soul's decision.
func (s *server) handleLLMJudge(w http.ResponseWriter, r *http.Request) {
	var req judgeCallRequest
	if !decodeCall(w, r, &req) {
		return
	}
	refuse := func(why string) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": why, "where": "soul"})
	}
	switch {
	case req.Evidence == nil:
		refuse("a judgement needs its evidence")
		return
	case len(req.Messages) > 0:
		refuse("a judgement is made from evidence, not messages")
		return
	case !judgePurpose(req.Purpose):
		refuse("a judgement's purpose is py.judge or one of its kinds")
		return
	case len(req.Look) > judge.MaxLook:
		refuse("a closer look names at most 6 terms")
		return
	}
	call := req.llmCallRequest
	for _, m := range judge.Messages(*req.Evidence, req.Look, req.Closer) {
		content, err := json.Marshal(m.Content)
		if err != nil {
			refuse("the evidence could not be worded")
			return
		}
		call.Messages = append(call.Messages, callMessage{Role: m.Role, Content: content})
	}
	s.makeCall(w, r, call, func(text string) any { return judge.Decide(text) })
}
