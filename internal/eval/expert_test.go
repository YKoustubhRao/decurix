package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/llm"
	"github.com/YKoustubhRao/decurix/internal/llmrec"
)

// scriptedHub answers the expert's calls in turn and keeps every body it was sent.
type scriptedHub struct {
	mu      sync.Mutex
	replies []llm.ChatResult
	bodies  [][]byte
}

func (h *scriptedHub) Configured(string) bool { return true }
func (h *scriptedHub) Status() map[string]llm.ProviderStatus {
	return map[string]llm.ProviderStatus{"azure": {Configured: true, Endpoint: "https://models.example.test"}}
}
func (h *scriptedHub) Resolve(string, string) (string, error) { return "big-1", nil }
func (h *scriptedHub) ChatResult(_ context.Context, _ string, body []byte) (llm.ChatResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.bodies = append(h.bodies, append([]byte{}, body...))
	r := h.replies[0]
	if len(h.replies) > 1 {
		h.replies = h.replies[1:]
	}
	return r, nil
}

func content(c string) llm.ChatResult {
	b, _ := json.Marshal(c)
	return llm.ChatResult{Status: 200, Deployment: "big-1", Body: []byte(fmt.Sprintf(`{"choices":[{"message":{"content":%s}}],"usage":{"prompt_tokens":30,"completion_tokens":4}}`, b))}
}

var expertCase = Case{ID: "needs-centre",
	Facts: []Fact{{ID: "centre", About: "which cost centre the bill is charged to", RefersTo: []string{"centre", "charge"}, Answer: "Charge it to Project Birch.", Required: yes()},
		{ID: "colour", About: "the folder colour", RefersTo: []string{"colour", "folder"}, Answer: "Blue.", Required: no()}},
	Expected: Expected{Outcome: OutcomeComplete, Graders: []Grader{{Kind: "mock_records", Collection: "bills",
		Records: json.RawMessage(`[{"number": {"id": "SECRET-EXPECTED-7731"}, "cost_centre": "Project Birch"}]`)}}},
	Oracle: []OracleStep{{Kind: StepRun, Line: "curl -d number=ORACLE-LINE-5521 x"}}}

func TestTheExpertPostsTheChosenFactsBytes(t *testing.T) {
	hub := &scriptedHub{replies: []llm.ChatResult{content(`{"fact": "centre"}`)}}
	rec := &recorder{}
	e := &Expert{Hub: hub, Recorder: rec, Deployment: "big-1"}
	c, err := e.ChooseFor(context.Background(), "practice-e1-a1", "t1", "Which cost centre should this go to?", expertCase.Facts)
	if err != nil || c.Fact != "centre" || c.Problem != "" {
		t.Fatalf("choice: %+v %v", c, err)
	}
	if got := Reply(expertCase, c.Fact); got != "Charge it to Project Birch." {
		t.Fatalf("reply: %q", got)
	}
	if len(rec.calls) != 1 || rec.calls[0].Purpose != ExpertPurpose || rec.calls[0].Caller != llmrec.CallerEval || rec.calls[0].Task != "t1" || rec.calls[0].Deployment != "big-1" {
		t.Fatalf("recorded: %+v", rec.calls)
	}
	var body map[string]any
	_ = json.Unmarshal(hub.bodies[0], &body)
	if body["temperature"] != float64(0) || body["model"] != "default" {
		t.Fatalf("body: %s", hub.bodies[0])
	}
}

func TestTheExpertsNoneOrGarbageGetsTheFixedSentence(t *testing.T) {
	for name, reply := range map[string]llm.ChatResult{
		"null":         content(`{"fact": null}`),
		"unknown id":   content(`{"fact": "weather"}`),
		"not json":     content(`centre, I think`),
		"empty":        {Status: 200, Body: []byte(`{}`), Deployment: "big-1"},
		"server error": {Status: 500, Body: []byte(`{"error": "x"}`), Deployment: "big-1"},
	} {
		e := &Expert{Hub: &scriptedHub{replies: []llm.ChatResult{reply}}, Deployment: "big-1"}
		c, _ := e.ChooseFor(context.Background(), "", "", "What is the date?", expertCase.Facts)
		if c.Fact != "" || Reply(expertCase, c.Fact) != DontKnow {
			t.Errorf("%s: %+v", name, c)
		}
	}
}

func TestTheExpertRetriesWithoutTemperatureOnce(t *testing.T) {
	hub := &scriptedHub{replies: []llm.ChatResult{
		{Status: 400, Body: []byte(`{"error": {"message": "Unsupported value: 'temperature' does not support 0"}}`), Deployment: "big-1"},
		content(`{"fact": "colour"}`),
	}}
	e := &Expert{Hub: hub, Deployment: "big-1"}
	c, err := e.ChooseFor(context.Background(), "", "", "Which folder?", expertCase.Facts)
	if err != nil || c.Fact != "colour" || len(hub.bodies) != 2 || strings.Contains(string(hub.bodies[1]), "temperature") {
		t.Fatalf("choice %+v, %d bodies", c, len(hub.bodies))
	}
	if _, err := e.ChooseFor(context.Background(), "", "", "Which folder?", expertCase.Facts); err != nil || strings.Contains(string(hub.bodies[2]), "temperature") {
		t.Fatal("the run forgot the deployment takes no temperature")
	}
}

func TestTheExpertNamesAnotherDeployment(t *testing.T) {
	r := content(`{"fact": "centre"}`)
	r.Deployment = "big-2"
	e := &Expert{Hub: &scriptedHub{replies: []llm.ChatResult{r}}, Deployment: "big-1"}
	c, _ := e.ChooseFor(context.Background(), "", "", "Which centre?", expertCase.Facts)
	if c.Fact != "centre" || !strings.Contains(c.Problem, modelChanged) {
		t.Fatalf("choice: %+v", c)
	}
}

// The expert is shown the question and the facts, never a grader, an expected record or the oracle (D9).
func TestTheExpertNeverSeesTheChecks(t *testing.T) {
	hub := &scriptedHub{replies: []llm.ChatResult{content(`{"fact": "centre"}`)}}
	e := &Expert{Hub: hub}
	if _, err := e.Choose(context.Background(), "Which cost centre?", expertCase.Facts); err != nil {
		t.Fatal(err)
	}
	body := string(hub.bodies[0])
	for _, leak := range []string{"SECRET-EXPECTED-7731", "ORACLE-LINE-5521", "mock_records", "expected", "oracle", "graders"} {
		if strings.Contains(body, leak) {
			t.Errorf("the expert's request holds %q: %s", leak, body)
		}
	}
	for _, shown := range []string{"Which cost centre?", "centre", "which cost centre the bill is charged to", "Charge it to Project Birch.", ExpertPrompt[:40]} {
		if !strings.Contains(body, strings.ReplaceAll(shown, `"`, `\"`)) {
			t.Errorf("the expert's request lacks %q", shown)
		}
	}
	// A case with no facts asks nobody.
	if c, err := e.Choose(context.Background(), "Anything?", nil); err != nil || c != "" || len(hub.bodies) != 1 {
		t.Fatalf("no facts: %q %v, %d calls", c, err, len(hub.bodies))
	}
}

// recorder keeps the calls it is given.
type recorder struct {
	mu    sync.Mutex
	calls []llmrec.Call
}

func (r *recorder) Record(_ context.Context, c llmrec.Call) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, c)
}
