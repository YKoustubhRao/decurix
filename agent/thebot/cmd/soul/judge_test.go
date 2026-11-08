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

// judgeCorpus is internal/judge's testdata/parity.json, recorded from the Python code: each case's evidence, the
// messages today's path sends for it, and the model's answers with the decision today's _parse makes of each.
type judgeCorpus struct {
	Cases []struct {
		Name     string            `json:"name"`
		Evidence json.RawMessage   `json:"evidence"`
		Messages []json.RawMessage `json:"messages"`
		LookUp   *string           `json:"look_up"`
		Answers  []struct {
			Text     string          `json:"text"`
			Decision json.RawMessage `json:"decision"`
		} `json:"answers"`
	} `json:"cases"`
}

func loadJudgeCorpus(t *testing.T) judgeCorpus {
	t.Helper()
	raw, err := os.ReadFile("../../internal/judge/testdata/parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var c judgeCorpus
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// judgeCallBody is a call as soul_call sends it: the same fields for /api/llm/call (with messages) and
// /api/llm/judge (with evidence, look and closer).
func judgeCallBody(t *testing.T, more map[string]any) string {
	t.Helper()
	body := map[string]any{"brain": "hosted", "purpose": "py.judge", "task": "t0123456789abcdef", "turn": "j42:7",
		"model": "", "max_tokens": 4000, "temperature": nil, "response_format": nil, "stop": []string{},
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

func jsonValue(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("%v: %.300s", err, raw)
	}
	return v
}

// TestTheJudgeRouteSendsTheBytesTodaysPathSends: for every recorded case, the call soul makes from the evidence and
// the call /api/llm/call makes from today's messages reach the proxy as the same bytes, with the same purpose - and
// soul's decision of the answer is the recorded one.
func TestTheJudgeRouteSendsTheBytesTodaysPathSends(t *testing.T) {
	for _, c := range loadJudgeCorpus(t).Cases {
		t.Run(c.Name, func(t *testing.T) {
			s, proxy, _ := callServer(t)
			var look []string
			closer := ""
			for i, messages := range c.Messages {
				if i > 0 {
					var first struct {
						Look []string `json:"look"`
					}
					if err := json.Unmarshal(c.Answers[0].Decision, &first); err != nil {
						t.Fatal(err)
					}
					look, closer = first.Look, *c.LookUp
				}
				proxy.answer = c.Answers[i].Text
				today := postCall(s, testAgentToken, llmCallPath, judgeCallBody(t, map[string]any{"messages": messages}))
				soul := postCall(s, testAgentToken, llmJudgePath, judgeCallBody(t, map[string]any{
					"evidence": c.Evidence, "look": look, "closer": closer}))
				if today.Code != http.StatusOK || soul.Code != http.StatusOK {
					t.Fatalf("call %d: today %d %s; soul %d %s", i+1, today.Code, today.Body.String(), soul.Code, soul.Body.String())
				}
				sent := proxy.raws[len(proxy.raws)-2:]
				if !bytes.Equal(sent[0], sent[1]) {
					t.Fatalf("call %d: the proxy got different bytes:\n today: %.1500s\n soul:  %.1500s", i+1, sent[0], sent[1])
				}
				heads := proxy.headers[len(proxy.headers)-2:]
				if heads[0].Get(llm.HeaderPurpose) != "py.judge" || heads[1].Get(llm.HeaderPurpose) != "py.judge" {
					t.Fatalf("purposes %q, %q", heads[0].Get(llm.HeaderPurpose), heads[1].Get(llm.HeaderPurpose))
				}
				answer := answerOf(t, soul)
				got, _ := json.Marshal(answer["decision"])
				if !reflect.DeepEqual(jsonValue(t, got), jsonValue(t, c.Answers[i].Decision)) {
					t.Fatalf("call %d: decision %s, recorded %s", i+1, got, c.Answers[i].Decision)
				}
				if answer["text"] != c.Answers[i].Text {
					t.Fatalf("call %d: text %q", i+1, answer["text"])
				}
				if _, has := answerOf(t, today)["decision"]; has {
					t.Fatal("/api/llm/call answered a decision")
				}
			}
		})
	}
}

// TestTheJudgeRouteRefusesWhatIsNotAJudgement: no evidence, messages in its place, another purpose or too many terms
// are soul's refusals (400, "where":"soul": the agent takes today's path); the call key does not open the route (the
// agent shows its token; a command with only the key goes today's way); nothing reaches the proxy.
func TestTheJudgeRouteRefusesWhatIsNotAJudgement(t *testing.T) {
	s, proxy, _ := callServer(t)
	evidence := map[string]any{"task": "Check the work.", "screen": false}
	for name, body := range map[string]string{
		"no evidence":  judgeCallBody(t, nil),
		"messages":     judgeCallBody(t, map[string]any{"evidence": evidence, "messages": []map[string]any{{"role": "user", "content": "x"}}}),
		"purpose":      judgeCallBody(t, map[string]any{"evidence": evidence, "purpose": "py.study"}),
		"soul purpose": judgeCallBody(t, map[string]any{"evidence": evidence, "purpose": "judgerish"}),
		"look":         judgeCallBody(t, map[string]any{"evidence": evidence, "look": []string{"a", "b", "c", "d", "e", "f", "g"}}),
		"brain":        judgeCallBody(t, map[string]any{"evidence": evidence, "brain": "local"}),
	} {
		rec := postCall(s, testAgentToken, llmJudgePath, body)
		if rec.Code != http.StatusBadRequest || answerOf(t, rec)["where"] != "soul" {
			t.Fatalf("%s: got %d %s", name, rec.Code, rec.Body.String())
		}
	}
	body := judgeCallBody(t, map[string]any{"evidence": evidence})
	if rec := postCall(s, "call-key-for-tests", llmJudgePath, body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the call key opened the judge route: %d", rec.Code)
	}
	if len(proxy.raws) != 0 {
		t.Fatalf("the proxy saw %d calls", len(proxy.raws))
	}
	if rec := postCall(s, testAgentToken, llmJudgePath, judgeCallBody(t, map[string]any{"evidence": evidence,
		"purpose": "py.judge.spot"})); rec.Code != http.StatusOK {
		t.Fatalf("py.judge.spot: %d %s", rec.Code, rec.Body.String())
	}
}

// TestAJudgementTheProviderFailedIsTheProvidersFailure: as /api/llm/call - its status, "where":"provider" - so the
// agent retries a 429 and maps the rest as it always has; no decision is made of no answer.
func TestAJudgementTheProviderFailedIsTheProvidersFailure(t *testing.T) {
	s, proxy, _ := callServer(t)
	proxy.status, proxy.message = http.StatusTooManyRequests, "slow down"
	rec := postCall(s, testAgentToken, llmJudgePath, judgeCallBody(t, map[string]any{
		"evidence": map[string]any{"task": "Check the work."}}))
	answer := answerOf(t, rec)
	if rec.Code != http.StatusTooManyRequests || answer["where"] != "provider" || answer["decision"] != nil {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

// TestAnAnswerSoulCannotReadIsAnErrorNotAVerdict: the decision is {"error", "cut_off"}, and the agent maps it to
// unsure as it maps its own parse's errors; never a done.
func TestAnAnswerSoulCannotReadIsAnErrorNotAVerdict(t *testing.T) {
	s, proxy, _ := callServer(t)
	proxy.answer = `{"verdict": "done", "confidence": 0.9, "seen": "the form is sav`
	rec := postCall(s, testAgentToken, llmJudgePath, judgeCallBody(t, map[string]any{
		"evidence": map[string]any{"task": "Check the work."}}))
	decision, _ := answerOf(t, rec)["decision"].(map[string]any)
	if rec.Code != http.StatusOK || decision["cut_off"] != true || decision["verdict"] != nil {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}
