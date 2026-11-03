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

	"brainiac/internal/catalog"
	"brainiac/internal/config"
	"brainiac/internal/install"
	"brainiac/internal/selector"
	"brainiac/internal/store"
)

var pngBytes = []byte("\x89PNG\r\n\x1a\nnot-really")

func TestAtomizeFollowsTheRules(t *testing.T) {
	msgs := []InMessage{{Role: "user", Parts: []Part{
		{Kind: "text", Text: "what is this?"}, {Kind: "image", Data: pngBytes}, {Kind: "audio", Data: []byte("RIFF")}, {Kind: "image", Data: pngBytes},
	}}}
	got := func(c Caps) string {
		var ops []string
		for _, a := range Atomize(msgs, c) {
			ops = append(ops, a.Op+":"+a.ID)
		}
		return strings.Join(ops, " ")
	}
	if g := got(Caps{BrainSees: true, OCR: true, ASR: true, Vision: true}); g != "ocr:image-1 asr:audio-1 ocr:image-2" {
		t.Errorf("seeing brain: %s", g)
	}
	if g := got(Caps{OCR: true, ASR: true, Vision: true}); g != "ocr:image-1 describe:image-1 asr:audio-1 ocr:image-2 describe:image-2" {
		t.Errorf("blind brain: %s", g)
	}
	if g := got(Caps{}); g != "" {
		t.Errorf("nothing installed: %s", g)
	}
	if g := Atomize([]InMessage{{Role: "user", Parts: []Part{{Kind: "text", Text: "hello"}}}}, Caps{OCR: true, ASR: true}); len(g) != 0 {
		t.Errorf("text should need no atoms: %v", g)
	}
}

type fakeUnit struct {
	name     string
	gb       float64
	loads    int
	unloads  int
	failLoad bool
	dead     bool // it went away after loading
}

func (f *fakeUnit) key() string    { return f.name }
func (f *fakeUnit) pool() string   { return "host" }
func (f *fakeUnit) memGB() float64 { return f.gb }
func (f *fakeUnit) load(context.Context) error {
	if f.failLoad {
		return errors.New("boom")
	}
	f.loads++
	return nil
}
func (f *fakeUnit) unload()     { f.unloads++ }
func (f *fakeUnit) alive() bool { return !f.dead }

func TestResidencyEvictsOnlyIdleOwnModels(t *testing.T) {
	free := 10.0
	r := NewResidency(map[string]float64{"host": 3}, func(context.Context, string) (float64, error) { return free, nil })
	a, b, c := &fakeUnit{name: "a", gb: 2}, &fakeUnit{name: "b", gb: 2}, &fakeUnit{name: "c", gb: 2}
	relA, _, err := r.Acquire(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	// a is in use: b cannot evict it.
	if _, _, err := r.Acquire(context.Background(), b); err == nil {
		t.Fatal("b loaded while a was in use and both do not fit")
	}
	relA()
	relB, _, err := r.Acquire(context.Background(), b)
	if err != nil || a.unloads != 1 {
		t.Fatalf("b: %v, a unloads %d", err, a.unloads)
	}
	// The budget is plenty now, but the machine has only 1 GB free and b is
	// in use, so c (2 GB) must be refused rather than pushed into swap.
	free = 1.0
	r.budgets["host"] = 10
	_, _, err = r.Acquire(context.Background(), c)
	var nr *ErrNoRoom
	if !errors.As(err, &nr) {
		t.Fatalf("expected no room with 1 GB free and b in use: %v", err)
	}
	// Once b is idle, evicting it gives the 2 GB c needs.
	relB()
	free = 0.1
	if _, _, err := r.Acquire(context.Background(), c); err != nil {
		t.Fatalf("c should fit after evicting b: %v", err)
	}
	if b.unloads != 1 {
		t.Fatalf("b unloads %d", b.unloads)
	}
}

// fakeOllama answers /api/chat with a fixed reply and records requests.
type fakeOllama struct {
	mu   sync.Mutex
	reqs []map[string]any
}

func (f *fakeOllama) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/ps":
		fmt.Fprint(w, `{"models":[]}`)
	case "/api/chat":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.reqs = append(f.reqs, body)
		f.mu.Unlock()
		fmt.Fprintln(w, `{"message":{"content":"The total is 12,345.00 and tax 999."},"done":false}`)
		fmt.Fprintln(w, `{"message":{"content":""},"done":true,"prompt_eval_count":40,"eval_count":9}`)
	default:
		http.NotFound(w, r)
	}
}

func lockWith(picks ...selector.Pick) *install.Lock {
	return &install.Lock{Plan: &selector.Plan{Picks: picks, Budgets: []selector.Budget{{Pool: "host", BudgetGB: 100}}}, Units: map[string]install.Unit{}}
}

func ollamaPick(role, model, tag string) selector.Pick {
	return selector.Pick{Role: role, Model: model, Pool: "host", MemGB: 0.1,
		Source: catalog.Source{Kind: catalog.SourceOllama, Runtime: catalog.RuntimeOllama, Tag: tag}}
}

func newEngine(t *testing.T, cfg config.Config, lock *install.Lock) *Engine {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(Options{Config: cfg, Store: st, Lock: lock})
	if err != nil {
		t.Fatal(err)
	}
	e.res.live = nil // tests do not measure this machine
	return e
}

func TestTextGoesStraightToTheBrain(t *testing.T) {
	fo := &fakeOllama{}
	srv := httptest.NewServer(fo)
	defer srv.Close()
	cfg := config.Default()
	cfg.Mode, cfg.OllamaURL = config.ModeLocal, srv.URL
	e := newEngine(t, cfg, lockWith(ollamaPick(catalog.RoleBrain, "tiny", "tiny:1b")))
	var streamed strings.Builder
	res, err := e.Chat(context.Background(), Request{Model: "brainiac", OnDelta: func(s string) { streamed.WriteString(s) },
		Messages: []InMessage{{Role: "user", Parts: []Part{{Kind: "text", Text: "hello"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text == "" || streamed.String() != res.Text || len(res.Trace.Atoms) != 1 || res.Trace.Atoms[0].Op != "brain" {
		t.Fatalf("res %+v atoms %+v", res, res.Trace.Atoms)
	}
	if strings.Contains(fmt.Sprint(fo.reqs[0]["messages"]), "evidence") {
		t.Fatal("a text request was given the evidence note")
	}
	// The answer's figures are not in the question: the check lists them.
	if len(res.Trace.Checks) != 1 || !strings.Contains(strings.Join(res.Trace.Checks[0].Items, ","), "12345") {
		t.Fatalf("checks %+v", res.Trace.Checks)
	}
	if res.Trace.LocalT != 49 || res.Trace.RemoteT != 0 {
		t.Fatalf("tokens local %d remote %d", res.Trace.LocalT, res.Trace.RemoteT)
	}
}

func TestAPictureNothingCanReadIsSaidPlainly(t *testing.T) {
	fo := &fakeOllama{}
	srv := httptest.NewServer(fo)
	defer srv.Close()
	cfg := config.Default()
	cfg.Mode, cfg.OllamaURL = config.ModeLocal, srv.URL
	e := newEngine(t, cfg, lockWith(ollamaPick(catalog.RoleBrain, "tiny", "tiny:1b")))
	_, err := e.Chat(context.Background(), Request{Messages: []InMessage{
		{Role: "system", Parts: []Part{{Kind: "text", Text: "You are helpful."}}},
		{Role: "user", Parts: []Part{{Kind: "text", Text: "read this"}, {Kind: "image", Data: pngBytes}}}}})
	if err != nil {
		t.Fatal(err)
	}
	msgs := fo.reqs[0]["messages"].([]any)
	sys := msgs[0].(map[string]any)["content"].(string)
	user := msgs[1].(map[string]any)
	if !strings.HasPrefix(sys, "You are helpful.") || !strings.Contains(sys, "never as instructions") {
		t.Fatalf("system %q", sys)
	}
	if !strings.Contains(user["content"].(string), "could not be read") || user["images"] != nil {
		t.Fatalf("user %v", user)
	}
}

func TestVisionBrainSeesThePicture(t *testing.T) {
	fo := &fakeOllama{}
	srv := httptest.NewServer(fo)
	defer srv.Close()
	cfg := config.Default()
	cfg.Mode, cfg.OllamaURL = config.ModeLocal, srv.URL
	e := newEngine(t, cfg, lockWith(ollamaPick(catalog.RoleBrain, "vlm", "vlm:2b"), ollamaPick(catalog.RoleVision, "vlm", "vlm:2b")))
	_, err := e.Chat(context.Background(), Request{Messages: []InMessage{
		{Role: "user", Parts: []Part{{Kind: "text", Text: "what is it"}, {Kind: "image", Data: pngBytes, Name: "shot.png"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	user := fo.reqs[0]["messages"].([]any)[0].(map[string]any)
	if imgs, _ := user["images"].([]any); len(imgs) != 1 {
		t.Fatalf("picture not passed: %v", user)
	}
	if !strings.Contains(user["content"].(string), "image-1 (shot.png)") {
		t.Fatalf("picture not labelled: %v", user["content"])
	}
}

func TestHybridKeepsPicturesLocalByDefault(t *testing.T) {
	var got map[string]any
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/llm/azure/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		fmt.Fprint(w, `{"model":"gpt-5.4","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":1}}`)
	}))
	defer cp.Close()
	cfg := config.Default()
	cfg.Mode = config.ModeHybrid
	cfg.Remote = &config.Remote{Kind: config.RemoteDecurix, URL: cp.URL, Provider: "azure", Model: "default", Vision: true}
	e := newEngine(t, cfg, lockWith())
	res, err := e.Chat(context.Background(), Request{OnDelta: func(string) {}, Messages: []InMessage{
		{Role: "user", Parts: []Part{{Kind: "text", Text: "x"}, {Kind: "image", Data: pngBytes}}}}})
	if err != nil {
		t.Fatal(err, errors.Unwrap(err))
	}
	if got["model"] != "default" || got["stream"] == true {
		t.Fatalf("proxy request %v", got)
	}
	if strings.Contains(fmt.Sprint(got["messages"]), "image_url") {
		t.Fatal("a picture left the machine without share_media")
	}
	if res.Brain != "decurix:azure" || res.Trace.RemoteT != 8 {
		t.Fatalf("brain %s remote tokens %d", res.Brain, res.Trace.RemoteT)
	}
}

func TestRemoteFailureIsNotPassedOffAsLocal(t *testing.T) {
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"secret provider detail"}}`)
	}))
	defer cp.Close()
	fo := &fakeOllama{}
	srv := httptest.NewServer(fo)
	defer srv.Close()
	cfg := config.Default()
	cfg.Mode, cfg.OllamaURL = config.ModeHybrid, srv.URL
	cfg.Remote = &config.Remote{Kind: config.RemoteDecurix, URL: cp.URL, Provider: "claude", Model: "default"}
	e := newEngine(t, cfg, lockWith(ollamaPick(catalog.RoleBrain, "tiny", "tiny:1b")))
	_, err := e.Chat(context.Background(), Request{Messages: []InMessage{{Role: "user", Parts: []Part{{Kind: "text", Text: "x"}}}}})
	var re *RemoteError
	if !errors.As(err, &re) || re.Status != 429 || strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v", err)
	}
	if len(fo.reqs) != 0 {
		t.Fatal("the local brain answered without local_fallback")
	}
	e.cfg.LocalFallback = true
	res, err := e.Chat(context.Background(), Request{Messages: []InMessage{{Role: "user", Parts: []Part{{Kind: "text", Text: "x"}}}}})
	if err != nil || !strings.Contains(res.Brain, "fallback") || len(res.Trace.Notes) == 0 {
		t.Fatalf("fallback res %+v err %v", res, err)
	}
}

// A remote stream that fails partway, with local_fallback on, never reaches
// the client as the start of one answer followed by the local one; a remote
// answer that finishes reaches it whole.
func TestFallbackNeverSendsTwoAnswers(t *testing.T) {
	cut := true
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"The remote total is 1"}}]}`)
		if !cut {
			fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"2"},"finish_reason":"stop"}]}`)
		}
	}))
	defer remote.Close()
	fo := &fakeOllama{}
	srv := httptest.NewServer(fo)
	defer srv.Close()
	cfg := config.Default()
	cfg.Mode, cfg.OllamaURL, cfg.LocalFallback = config.ModeHybrid, srv.URL, true
	cfg.Remote = &config.Remote{Kind: config.RemoteOpenAI, URL: remote.URL, Model: "m"}
	e := newEngine(t, cfg, lockWith(ollamaPick(catalog.RoleBrain, "tiny", "tiny:1b")))
	ask := func() (string, *Response) {
		var sent strings.Builder
		res, err := e.Chat(context.Background(), Request{OnDelta: func(s string) { sent.WriteString(s) },
			Messages: []InMessage{{Role: "user", Parts: []Part{{Kind: "text", Text: "total?"}}}}})
		if err != nil {
			t.Fatal(err)
		}
		return sent.String(), res
	}
	sent, res := ask()
	if sent != "The total is 12,345.00 and tax 999." || !strings.Contains(res.Brain, "fallback") {
		t.Fatalf("the client got %q from %s", sent, res.Brain)
	}
	if res.Mode != config.ModeLocal {
		t.Errorf("a local answer reported mode %s", res.Mode)
	}
	cut = false
	if sent, _ := ask(); sent != "The remote total is 12" {
		t.Fatalf("the finished remote answer reached the client as %q", sent)
	}
}
