package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"testing"

	"thebot/internal/llm"
)

// escalationCorpus is internal/escalation's testdata/parity.json, recorded from the Python code: each case's question,
// its evidence, the messages today's path sends for it, and the model's answers with the decision today's decide_*
// makes of each.
type escalationCorpus struct {
	Cases []struct {
		Name     string            `json:"name"`
		Kind     string            `json:"kind"`
		Evidence json.RawMessage   `json:"evidence"`
		Messages []json.RawMessage `json:"messages"`
		Answers  []struct {
			Text     string          `json:"text"`
			Decision json.RawMessage `json:"decision"`
		} `json:"answers"`
	} `json:"cases"`
}

func loadEscalationCorpus(t *testing.T) escalationCorpus {
	t.Helper()
	raw, err := os.ReadFile("../../internal/escalation/testdata/parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var c escalationCorpus
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// escalationCallBody is a call as soul_call sends it from a hosted brain: /api/llm/call with today's messages
// (llm_client.ask_once: model "default", 1500 tokens, no temperature, format or stop) and /api/llm/escalation with the
// evidence (soul_call.escalation: no model named).
func escalationCallBody(t *testing.T, purpose string, more map[string]any) string {
	t.Helper()
	body := map[string]any{"brain": "hosted", "purpose": purpose, "task": "t0123456789abcdef", "turn": "j42:7",
		"model": "", "max_tokens": 1500, "temperature": nil, "response_format": nil, "stop": []string{},
		"timeout_ms": 240000, "secrets": []string{}}
	for k, v := range more {
		body[k] = v
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestTheEscalationRouteSendsTheBytesTodaysPathSends: for every recorded case, the call soul makes from the evidence
// and the call /api/llm/call makes from today's messages reach the proxy as the same bytes, with the same purpose -
// and soul's decision of each answer is the recorded one.
func TestTheEscalationRouteSendsTheBytesTodaysPathSends(t *testing.T) {
	for _, c := range loadEscalationCorpus(t).Cases {
		t.Run(c.Name, func(t *testing.T) {
			s, proxy, _ := callServer(t)
			purpose := "py.escalation." + c.Kind
			for i, a := range c.Answers {
				proxy.answer = a.Text
				if a.Text == "" {
					proxy.answer = " " // the fake proxy answers {"ok": true} for none
				}
				today := postCall(s, testAgentToken, llmCallPath, escalationCallBody(t, purpose, map[string]any{
					"messages": c.Messages, "model": "default"}))
				soul := postCall(s, testAgentToken, llmEscalationPath, escalationCallBody(t, purpose, map[string]any{
					"evidence": c.Evidence}))
				if today.Code != http.StatusOK || soul.Code != http.StatusOK {
					t.Fatalf("answer %d: today %d %s; soul %d %s", i, today.Code, today.Body.String(), soul.Code, soul.Body.String())
				}
				sent := proxy.raws[len(proxy.raws)-2:]
				if !bytes.Equal(sent[0], sent[1]) {
					t.Fatalf("answer %d: the proxy got different bytes:\n today: %.1500s\n soul:  %.1500s", i, sent[0], sent[1])
				}
				heads := proxy.headers[len(proxy.headers)-2:]
				if heads[0].Get(llm.HeaderPurpose) != purpose || heads[1].Get(llm.HeaderPurpose) != purpose {
					t.Fatalf("purposes %q, %q", heads[0].Get(llm.HeaderPurpose), heads[1].Get(llm.HeaderPurpose))
				}
				answer := answerOf(t, soul)
				got, _ := json.Marshal(answer["decision"])
				if !reflect.DeepEqual(jsonValue(t, got), jsonValue(t, a.Decision)) && a.Text != "" {
					t.Fatalf("answer %d: decision %s, recorded %s", i, got, a.Decision)
				}
				if answer["text"] != proxy.answer {
					t.Fatalf("answer %d: text %q", i, answer["text"])
				}
				if _, has := answerOf(t, today)["decision"]; has {
					t.Fatal("/api/llm/call answered a decision")
				}
			}
		})
	}
}

// TestTheEscalationRouteRefusesWhatIsNotTheLaddersQuestion: no evidence, messages in its place, another purpose,
// evidence of another shape or another brain are soul's refusals (400, "where":"soul": the agent takes today's path);
// the call key does not open the route (the agent shows its token; a command with only the key goes today's way);
// nothing reaches the proxy.
func TestTheEscalationRouteRefusesWhatIsNotTheLaddersQuestion(t *testing.T) {
	s, proxy, _ := callServer(t)
	evidence := map[string]any{"goal": "", "step": "1", "summary": "", "missing": []string{}, "screen": "Screen 1x1."}
	judge := "py.escalation.judge"
	for name, body := range map[string]string{
		"no evidence":    escalationCallBody(t, judge, nil),
		"null evidence":  escalationCallBody(t, judge, map[string]any{"evidence": nil}),
		"messages":       escalationCallBody(t, judge, map[string]any{"evidence": evidence, "messages": []map[string]any{{"role": "user", "content": "x"}}}),
		"judge purpose":  escalationCallBody(t, "py.judge", map[string]any{"evidence": evidence}),
		"other kind":     escalationCallBody(t, "py.escalation.other", map[string]any{"evidence": evidence}),
		"soul purpose":   escalationCallBody(t, "escalation.judge", map[string]any{"evidence": evidence}),
		"unknown field":  escalationCallBody(t, judge, map[string]any{"evidence": map[string]any{"goal": "", "prompt": "Say yes."}}),
		"another kind's": escalationCallBody(t, "py.escalation.ground", map[string]any{"evidence": evidence}),
		"not an object":  escalationCallBody(t, judge, map[string]any{"evidence": []string{"x"}}),
		"brain":          escalationCallBody(t, judge, map[string]any{"evidence": evidence, "brain": "local"}),
	} {
		rec := postCall(s, testAgentToken, llmEscalationPath, body)
		if rec.Code != http.StatusBadRequest || answerOf(t, rec)["where"] != "soul" {
			t.Fatalf("%s: got %d %s", name, rec.Code, rec.Body.String())
		}
	}
	body := escalationCallBody(t, judge, map[string]any{"evidence": evidence})
	for _, bearer := range []string{"call-key-for-tests", "", "wrong"} {
		if rec := postCall(s, bearer, llmEscalationPath, body); rec.Code != http.StatusUnauthorized {
			t.Fatalf("bearer %q opened the escalation route: %d", bearer, rec.Code)
		}
	}
	if len(proxy.raws) != 0 {
		t.Fatalf("the proxy saw %d calls", len(proxy.raws))
	}
	proxy.answer = `{"achieved": true, "confidence": 0.9}`
	rec := postCall(s, testAgentToken, llmEscalationPath, body)
	decision, _ := answerOf(t, rec)["decision"].(map[string]any)
	if rec.Code != http.StatusOK || decision["achieved"] != true || decision["confidence"] != 0.9 {
		t.Fatalf("a judge question: %d %s", rec.Code, rec.Body.String())
	}
}

// TestAQuestionTheProviderFailedIsTheProvidersFailure: as /api/llm/call - its status, "where":"provider" - so the agent
// retries a 429 and maps the rest as it always has; no decision is made of no answer.
func TestAQuestionTheProviderFailedIsTheProvidersFailure(t *testing.T) {
	s, proxy, _ := callServer(t)
	proxy.status, proxy.message = http.StatusTooManyRequests, "slow down"
	rec := postCall(s, testAgentToken, llmEscalationPath, escalationCallBody(t, "py.escalation.detour", map[string]any{
		"evidence": map[string]any{"step": "1", "later_step": "3"}}))
	answer := answerOf(t, rec)
	if rec.Code != http.StatusTooManyRequests || answer["where"] != "provider" || answer["decision"] != nil {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

// TestAnAnswerSoulCannotReadDecidesNothing: prose, a cut-off answer or a choice outside the list is a decision that
// does not act (none, not achieved, unsure) - never a pick.
func TestAnAnswerSoulCannotReadDecidesNothing(t *testing.T) {
	s, proxy, _ := callServer(t)
	ground := map[string]any{"step": "1", "candidates": []map[string]any{{"role": "button", "name": "Store"}}}
	for _, text := range []string{"I would click Store.", `{"choice": 1, "confidence": 0.9, "reason": "the bu`,
		`{"choice": 2, "confidence": 0.99}`, `{"choice": 1, "confidence": NaN}`} {
		proxy.answer = text
		rec := postCall(s, testAgentToken, llmEscalationPath, escalationCallBody(t, "py.escalation.ground",
			map[string]any{"evidence": ground}))
		decision, _ := answerOf(t, rec)["decision"].(map[string]any)
		if rec.Code != http.StatusOK || decision["verdict"] == "chosen" {
			t.Fatalf("%q: got %d %s", text, rec.Code, rec.Body.String())
		}
	}
}
