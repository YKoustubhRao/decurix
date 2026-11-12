package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"brainiac/internal/backend"
	"brainiac/internal/catalog"
	"brainiac/internal/config"
	"brainiac/internal/evals"
	"brainiac/internal/selector"
)

// taggedOllama answers each model tag with its own name, so a test can see who answered.
type taggedOllama struct {
	mu   sync.Mutex
	seen []string
}

func (f *taggedOllama) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/ps":
		fmt.Fprint(w, `{"models":[]}`)
	case "/api/chat":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		tag, _ := body["model"].(string)
		f.mu.Lock()
		f.seen = append(f.seen, tag)
		f.mu.Unlock()
		fmt.Fprintf(w, "{\"message\":{\"content\":\"answer from %s\"},\"done\":true,\"prompt_eval_count\":1,\"eval_count\":1}\n", tag)
	default:
		http.NotFound(w, r)
	}
}

func domainEngine(t *testing.T, mode string, remote *httptest.Server) (*Engine, *taggedOllama) {
	t.Helper()
	fo := &taggedOllama{}
	srv := httptest.NewServer(fo)
	t.Cleanup(srv.Close)
	cfg := config.Default()
	cfg.Mode, cfg.OllamaURL = mode, srv.URL
	if remote != nil {
		cfg.Remote = &config.Remote{Kind: config.RemoteDecurix, URL: remote.URL, Provider: "azure", Model: "default"}
	}
	e := newEngine(t, cfg, lockWith(
		ollamaPick(catalog.RoleBrain, "brain-2b", "brain:2b"),
		selector.Pick{Role: "math", Model: "math-1.5b", Pool: "host", MemGB: 0.1,
			Source: catalog.Source{Kind: catalog.SourceOllama, Runtime: catalog.RuntimeOllama, Tag: "math:1.5b"}},
	))
	return e, fo
}

func ask(t *testing.T, e *Engine, model, domain, text string) *Response {
	t.Helper()
	res, err := e.Chat(context.Background(), Request{Model: model, Domain: domain, Messages: []InMessage{{Role: "user", Parts: []Part{{Kind: "text", Text: text}}}}})
	if err != nil {
		t.Fatalf("%s: %v", text, err)
	}
	return res
}

func TestMathGoesToTheMathSpecialistLocally(t *testing.T) {
	e, _ := domainEngine(t, config.ModeLocal, nil)
	saveScores(t, e, map[string]evals.Score{"math-1.5b": {N: 100, Correct: 95}, "brain-2b": {N: 100, Correct: 80}})
	res := ask(t, e, "brainiac", "", "Solve 5x + 7 = 2x - 8")
	if res.Brain != "math-1.5b" || res.Trace.Domain != "math" {
		t.Fatalf("brain %s domain %s notes %v", res.Brain, res.Trace.Domain, res.Trace.Notes)
	}
	if res := ask(t, e, "brainiac", "", "write a short poem about the monsoon"); res.Brain != "brain-2b" {
		t.Fatalf("a poem went to %s", res.Brain)
	}
	// A field with no specialist installed stays with the brain, and says so.
	res = ask(t, e, "brainiac", "law", "Is a verbal agreement binding?")
	if res.Brain != "brain-2b" || !strings.Contains(strings.Join(res.Trace.Notes, " "), "no law specialist") {
		t.Fatalf("law: brain %s notes %v", res.Brain, res.Trace.Notes)
	}
	// Naming the field or the field model routes without the router.
	if res := ask(t, e, "brainiac-math", "", "hello"); res.Brain != "math-1.5b" {
		t.Fatalf("brainiac-math went to %s", res.Brain)
	}
	// A concrete model name is that model alone.
	if res := ask(t, e, "brain-2b", "", "Solve 5x + 7 = 2x - 8"); res.Brain != "brain-2b" {
		t.Fatalf("a named model was rerouted to %s", res.Brain)
	}
}

func TestMeasuredWorseSpecialistLosesItsField(t *testing.T) {
	e, _ := domainEngine(t, config.ModeLocal, nil)
	r := &evals.Results{}
	r.Set("math-1.5b", "math", evals.Score{N: 30, Correct: 12})
	r.Set("brain-2b", "math", evals.Score{N: 30, Correct: 20})
	if err := r.Save(e.st.Path("evals.json")); err != nil {
		t.Fatal(err)
	}
	res := ask(t, e, "brainiac", "", "Solve 5x + 7 = 2x - 8")
	if res.Brain != "brain-2b" || !strings.Contains(strings.Join(res.Trace.Notes, " "), "measured worse") {
		t.Fatalf("brain %s notes %v", res.Brain, res.Trace.Notes)
	}
}

// remoteEngine is a hybrid engine whose remote brain always answers.
func remoteEngine(t *testing.T) *Engine {
	t.Helper()
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"model":"gpt-5.4","choices":[{"message":{"content":"remote answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":100}}`)
	}))
	t.Cleanup(cp.Close)
	e, _ := domainEngine(t, config.ModeHybrid, cp)
	return e
}

func saveScores(t *testing.T, e *Engine, scores map[string]evals.Score) {
	t.Helper()
	r := &evals.Results{}
	for m, s := range scores {
		if s.At.IsZero() {
			s.At = time.Now().Add(-time.Hour)
		}
		if s.Decided == 0 {
			s.Decided, s.DecidedCorrect = s.N, s.Correct // every case decided by a stated value
		}
		r.Set(m, "math", s)
		if m != RemoteEvalName {
			r.Set(RoutedEvalName(m), "math", s) // a router that files every case right
		}
	}
	if err := r.Save(e.st.Path("evals.json")); err != nil {
		t.Fatal(err)
	}
	// The cache compares modification times, which a fast test can repeat.
	e.evals.res = nil
}

const mathAsk = "Solve 5x + 7 = 2x - 8"

func TestASmallPerfectScoreDoesNotTakeAFieldFromThePerfectRemote(t *testing.T) {
	e := remoteEngine(t)
	saveScores(t, e, map[string]evals.Score{"math-1.5b": {N: 20, Correct: 20}, RemoteEvalName: {N: 20, Correct: 20}})
	res := ask(t, e, "brainiac", "", mathAsk)
	if res.Brain != "decurix:azure" || !strings.Contains(strings.Join(res.Trace.Notes, " "), "can claim only") {
		t.Fatalf("brain %s notes %v", res.Brain, res.Trace.Notes)
	}
}

func TestTheLocalBrainCanEarnAFieldToo(t *testing.T) {
	e := remoteEngine(t)
	saveScores(t, e, map[string]evals.Score{"brain-2b": {N: 100, Correct: 99, MeanMs: 900}, RemoteEvalName: {N: 100, Correct: 92}})
	res := ask(t, e, "brainiac", "", mathAsk)
	if res.Brain != "brain-2b" || res.Trace.RemoteT != 0 || res.Trace.Domain != "math" {
		t.Fatalf("brain %s remote tokens %d", res.Brain, res.Trace.RemoteT)
	}
}

func TestAmongEarnedPathsTheFasterAnswers(t *testing.T) {
	e := remoteEngine(t)
	saveScores(t, e, map[string]evals.Score{"math-1.5b": {N: 100, Correct: 99, MeanMs: 90000}, "brain-2b": {N: 100, Correct: 98, MeanMs: 4000}, RemoteEvalName: {N: 100, Correct: 92}})
	if res := ask(t, e, "brainiac", "", mathAsk); res.Brain != "brain-2b" {
		t.Fatalf("brain %s", res.Brain)
	}
}

func TestAPathTooSlowForTheLimitStaysRemote(t *testing.T) {
	e := remoteEngine(t)
	e.cfg.MaxLocalSeconds = 60
	saveScores(t, e, map[string]evals.Score{"math-1.5b": {N: 100, Correct: 99, MeanMs: 90000}, RemoteEvalName: {N: 100, Correct: 92}})
	res := ask(t, e, "brainiac", "", mathAsk)
	if res.Brain != "decurix:azure" || !strings.Contains(strings.Join(res.Trace.Notes, " "), "max_local_seconds") {
		t.Fatalf("brain %s notes %v", res.Brain, res.Trace.Notes)
	}
}

func TestARequestTooLongForTheLocalWindowStaysRemote(t *testing.T) {
	e := remoteEngine(t)
	saveScores(t, e, map[string]evals.Score{"math-1.5b": {N: 100, Correct: 99}, RemoteEvalName: {N: 100, Correct: 92}})
	long := mathAsk + " " + strings.Repeat("Solve for x. ", 5000)
	if res := ask(t, e, "brainiac", "math", long); res.Brain != "math-1.5b" {
		t.Fatalf("an explicit field went to %s", res.Brain)
	}
	if u, why := e.earnedPath("math", Request{Messages: []InMessage{{Role: "user", Parts: []Part{{Kind: "text", Text: long}}}}}); u != nil || !strings.Contains(why, "too long") {
		t.Fatalf("%v %s", u, why)
	}
}

func TestOneReportedMissSendsThePathBackUntilMeasuredAgain(t *testing.T) {
	e := remoteEngine(t)
	saveScores(t, e, map[string]evals.Score{"math-1.5b": {N: 100, Correct: 99}, RemoteEvalName: {N: 100, Correct: 92}})
	res := ask(t, e, "brainiac", "", mathAsk)
	if res.Brain != "math-1.5b" {
		t.Fatalf("earned: brain %s", res.Brain)
	}
	// A pass earns nothing and takes nothing away.
	if n, err := e.RecordOutcome(res.Trace.ID, time.Time{}, true); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if res := ask(t, e, "brainiac", "", mathAsk); res.Brain != "math-1.5b" {
		t.Fatalf("after a pass: brain %s", res.Brain)
	}
	// A whole task's verdict, by time: everything local since then.
	if n, err := e.RecordOutcome("", time.Now().Add(-time.Minute), false); err != nil || n == 0 {
		t.Fatalf("%d %v", n, err)
	}
	res = ask(t, e, "brainiac", "", mathAsk)
	if res.Brain != "decurix:azure" || !strings.Contains(strings.Join(res.Trace.Notes, " "), "reported wrong") {
		t.Fatalf("after a miss: brain %s notes %v", res.Brain, res.Trace.Notes)
	}
	// The remote answer's own verdict does not count against a local model.
	before := e.outcomes.Get("math-1.5b", "math").Missed
	if _, err := e.RecordOutcome(res.Trace.ID, time.Time{}, false); err != nil {
		t.Fatal(err)
	}
	if got := e.outcomes.Get("math-1.5b", "math").Missed; got != before {
		t.Fatalf("a remote answer's miss was filed against the local model: %d", got)
	}
	// Measured again after the miss, it earns the field back.
	saveScores(t, e, map[string]evals.Score{"math-1.5b": {N: 100, Correct: 99, At: time.Now().Add(time.Second)}, RemoteEvalName: {N: 100, Correct: 92}})
	if res := ask(t, e, "brainiac", "", mathAsk); res.Brain != "math-1.5b" {
		t.Fatalf("measured again: brain %s", res.Brain)
	}
	if _, err := e.RecordOutcome("no-such-trace", time.Time{}, false); !errors.Is(err, ErrNoTrace) {
		t.Fatalf("%v", err)
	}
	// It all shows, per field.
	paths := e.Paths()["math"]
	if len(paths) != 2 || paths[0].Model != "math-1.5b" || !paths[0].Earned || paths[0].Live.Missed == 0 {
		t.Fatalf("%+v", paths)
	}
}

// Locally, a specialist that is merely as good as the brain on the cases
// does not take the field (it is not proven at least as good); one clearly
// better does, however slow.
func TestLocallyASpecialistTakesAFieldOnlyWhenProvenAtLeastAsGood(t *testing.T) {
	e, _ := domainEngine(t, config.ModeLocal, nil)
	saveScores(t, e, map[string]evals.Score{"brain-2b": {N: 100, Correct: 99, MeanMs: 4000}, "math-1.5b": {N: 100, Correct: 99, MeanMs: 900000}})
	res := ask(t, e, "brainiac", "", mathAsk)
	if res.Brain != "brain-2b" || !strings.Contains(strings.Join(res.Trace.Notes, " "), "not proven") {
		t.Fatalf("brain %s notes %v", res.Brain, res.Trace.Notes)
	}
	saveScores(t, e, map[string]evals.Score{"brain-2b": {N: 100, Correct: 80, MeanMs: 4000}, "math-1.5b": {N: 100, Correct: 95, MeanMs: 900000}})
	if res := ask(t, e, "brainiac", "", mathAsk); res.Brain != "math-1.5b" {
		t.Fatalf("speed beat quality: brain %s", res.Brain)
	}
}

func TestWithARemoteBrainTheSpecialistMustEarnTheField(t *testing.T) {
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"model":"gpt-5.4","choices":[{"message":{"content":"remote answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer cp.Close()
	e, _ := domainEngine(t, config.ModeHybrid, cp)
	res := ask(t, e, "brainiac", "", "Solve 5x + 7 = 2x - 8")
	if res.Brain != "decurix:azure" || res.Trace.Domain != "math" {
		t.Fatalf("unmeasured: brain %s domain %s", res.Brain, res.Trace.Domain)
	}
	saveScores(t, e, map[string]evals.Score{"math-1.5b": {N: 100, Correct: 99}, RemoteEvalName: {N: 100, Correct: 90}})
	res = ask(t, e, "brainiac", "", "Solve 5x + 7 = 2x - 8")
	if res.Brain != "math-1.5b" || res.Trace.RemoteT != 0 {
		t.Fatalf("measured at least as good: brain %s remote tokens %d", res.Brain, res.Trace.RemoteT)
	}
	// The remote brain alone stays reachable by name, for evals.
	if res := ask(t, e, RemoteEvalName, "", "Solve 5x + 7 = 2x - 8"); res.Brain != "decurix:azure" {
		t.Fatalf("remote by name went to %s", res.Brain)
	}
}

// The router's guess never hands a request to a specialist nobody measured:
// the brain answers, and naming the field still reaches the specialist.
func TestAnUnmeasuredSpecialistAnswersOnlyWhenNamed(t *testing.T) {
	e, _ := domainEngine(t, config.ModeLocal, nil)
	res := ask(t, e, "brainiac", "", mathAsk)
	if res.Brain != "brain-2b" || res.Trace.Domain != "math" || !strings.Contains(strings.Join(res.Trace.Notes, " "), "not proven") {
		t.Fatalf("unmeasured, routed: brain %s notes %v", res.Brain, res.Trace.Notes)
	}
	if res := ask(t, e, "brainiac", "math", mathAsk); res.Brain != "math-1.5b" {
		t.Fatalf("named: brain %s", res.Brain)
	}
	// Measured only on cases that decide nothing, it still has not earned it.
	r := &evals.Results{}
	r.Set("math-1.5b", "math", evals.Score{N: 100, Correct: 100})
	r.Set("brain-2b", "math", evals.Score{N: 100, Correct: 50})
	if err := r.Save(e.st.Path("evals.json")); err != nil {
		t.Fatal(err)
	}
	e.evals.res = nil
	if res := ask(t, e, "brainiac", "", mathAsk); res.Brain != "brain-2b" {
		t.Fatalf("contains-only scores earned the field for %s", res.Brain)
	}
}

// A path earned on single questions does not take a conversation, an agent's
// long system prompt or a picture whose last turn looks like its field.
func TestAnEarnedPathServesOnlyRequestsShapedLikeItsCases(t *testing.T) {
	e := remoteEngine(t)
	saveScores(t, e, map[string]evals.Score{"math-1.5b": {N: 100, Correct: 99}, RemoteEvalName: {N: 100, Correct: 92}})
	if res := ask(t, e, "brainiac", "", mathAsk); res.Brain != "math-1.5b" || res.Trace.Shape == nil || res.Trace.Shape.UserTurns != 1 {
		t.Fatalf("a single question: brain %s shape %+v", res.Brain, res.Trace.Shape)
	}
	text := func(role, s string) InMessage { return InMessage{Role: role, Parts: []Part{{Kind: "text", Text: s}}} }
	for name, msgs := range map[string][]InMessage{
		"conversation":  {text("user", "I have a ledger to check."), text("assistant", "Send it."), text("user", mathAsk)},
		"agent prompt":  {text("system", strings.Repeat("You are an office worker with tools. ", 40)), text("user", mathAsk)},
		"with an image": {{Role: "user", Parts: []Part{{Kind: "text", Text: mathAsk}, {Kind: "image", Data: pngBytes}}}},
	} {
		res, err := e.Chat(context.Background(), Request{Model: "brainiac", Messages: msgs})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.Brain != "decurix:azure" {
			t.Errorf("%s went to %s: %v", name, res.Brain, res.Trace.Notes)
		}
	}
}

// A specialist proven by name on its field earns nothing for routed traffic
// when the router sends it questions of other fields: what is measured is
// the path that is served. Naming the field still reaches it.
func TestAPathProvenByNameButMisroutedDoesNotEarn(t *testing.T) {
	e := remoteEngine(t)
	byName := evals.Score{N: 100, Correct: 99, Decided: 100, DecidedCorrect: 99, At: time.Now().Add(-time.Hour)}
	// Half of what the router sends to math was office questions it misfiled.
	routed := evals.Score{N: 200, Correct: 99, Decided: 200, DecidedCorrect: 99, At: time.Now().Add(-time.Hour)}
	r := &evals.Results{}
	r.Set("math-1.5b", "math", byName)
	r.Set(RoutedEvalName("math-1.5b"), "math", routed)
	r.Set(RemoteEvalName, "math", evals.Score{N: 100, Correct: 92, Decided: 100, DecidedCorrect: 92})
	if err := r.Save(e.st.Path("evals.json")); err != nil {
		t.Fatal(err)
	}
	e.evals.res = nil
	if res := ask(t, e, "brainiac", "", mathAsk); res.Brain != "decurix:azure" || !strings.Contains(strings.Join(res.Trace.Notes, " "), "as routed") {
		t.Fatalf("misrouted traffic earned the field for %s: %v", res.Brain, res.Trace.Notes)
	}
	if res := ask(t, e, "brainiac", "math", mathAsk); res.Brain != "math-1.5b" {
		t.Fatalf("the named field went to %s", res.Brain)
	}
	// Routed as well as by name, it earns.
	r.Set(RoutedEvalName("math-1.5b"), "math", byName)
	if err := r.Save(e.st.Path("evals.json")); err != nil {
		t.Fatal(err)
	}
	e.evals.res = nil
	if res := ask(t, e, "brainiac", "", mathAsk); res.Brain != "math-1.5b" {
		t.Fatalf("a well-routed path did not earn: %s", res.Brain)
	}
}

// A path fast on its eval's short questions can still be slow on a request:
// with a time limit set, the request's own estimate (its length at the
// speeds the model showed here) keeps it with the remote brain.
func TestARequestTheLocalPathWouldTakeTooLongOnStaysRemote(t *testing.T) {
	e := remoteEngine(t)
	e.cfg.MaxLocalSeconds = 60
	saveScores(t, e, map[string]evals.Score{"math-1.5b": {N: 100, Correct: 99, MeanMs: 5000}, RemoteEvalName: {N: 100, Correct: 92}})
	// Recent calls showed 20 prompt tokens/s and 5 answer tokens/s.
	e.speed.record("math-1.5b", backend.Timings{PromptN: 2000, PromptMs: 100000, PredictedN: 500, PredictedMs: 100000}, nil)
	short := Request{Messages: []InMessage{{Role: "user", Parts: []Part{{Kind: "text", Text: mathAsk}}}}, MaxTokens: 100}
	if u, why := e.earnedPath("math", short); u == nil || u.model != "math-1.5b" {
		t.Fatalf("a short request: %v %s", u, why)
	}
	// 3000 characters is about 1000 tokens: 50 s to read, 20 s to write 100.
	long := Request{Messages: []InMessage{{Role: "user", Parts: []Part{{Kind: "text", Text: mathAsk + " " + strings.Repeat("x = 1. ", 430)}}}}, MaxTokens: 100}
	if u, why := e.earnedPath("math", long); u != nil || !strings.Contains(why, "would take about") {
		t.Fatalf("a long request: %v %s", u, why)
	}
}
