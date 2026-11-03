package practice

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/qualification"
	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

const testImage = "sha256:aaaa"

// ---- fakes ----

type fakeChat struct {
	mu       sync.Mutex
	calls    []string
	answer   string
	answers  []string // answered in turn before answer
	usage    int64
	disabled bool
}

func (f *fakeChat) Configured(p string) bool { return !f.disabled && p == "azure" }

func (f *fakeChat) Chat(_ context.Context, provider string, body []byte) (int, []byte, http.Header, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, provider+" "+string(body))
	answer := f.answer
	if len(f.answers) > 0 {
		answer, f.answers = f.answers[0], f.answers[1:]
	}
	content, _ := json.Marshal(answer)
	return 200, []byte(fmt.Sprintf(`{"choices":[{"message":{"content":%s}}],"usage":{"total_tokens":%d}}`, content, f.usage)), nil, nil
}

type fakeWorkers struct {
	brain    string
	agentSHA string
}

func (w *fakeWorkers) Get(_ context.Context, id string) (desktop.Desktop, error) {
	return desktop.Desktop{ID: id, Harness: "thebot", Brain: w.brain, Model: "big", MemoryMB: 4096}, nil
}
func (w *fakeWorkers) QualificationImage(context.Context, string) (string, error) {
	return testImage, nil
}
func (w *fakeWorkers) PracticeAgentTree(context.Context, string) ([]byte, string, error) {
	return []byte("tree"), w.agentSHA, nil
}
func (w *fakeWorkers) PracticeSnapshot(context.Context, string) ([]byte, string, error) {
	return tarOf(map[string]string{"experience/x.json": "{}"}), "snap", nil
}

func tarOf(files map[string]string) []byte {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for n, t := range files {
		_ = tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(t))})
		_, _ = tw.Write([]byte(t))
	}
	_ = tw.Close()
	return b.Bytes()
}

// fakeOffline is the offline container running a generator: its output is a
// pure function of the seed unless told otherwise.
type fakeOffline struct {
	mu        sync.Mutex
	runs      []int64
	drift     bool // the same seed gives different bytes
	fixed     bool // every seed gives the same cases
	missing   bool // a case names a file nobody wrote
	failSeeds map[int64]bool
	badCode   string // a generate.py that writes a file name the wrapper refuses
	mockOut   string // what the mock check prints; "[]" when empty
	mockCalls []qualification.Case
}

func officeCases(seed int64) ([]byte, map[string][]byte) {
	n := fmt.Sprint(seed)
	cases := []map[string]any{
		{"id": "ordinary-1", "category": "ordinary", "request": "Total ~/work/practice/ordinary-1/in.txt into out.txt", "files": []string{"in.txt"}, "expected": "complete",
			"checks": []map[string]any{{"kind": "text_equals", "file": "out.txt", "text": "total " + n}}},
		{"id": "varied-1", "category": "varied", "request": "Total in.txt", "files": []string{"in.txt"}, "expected": "complete",
			"checks": []map[string]any{{"kind": "text_equals", "file": "out.txt", "text": "total 2" + n}}},
		{"id": "missing-1", "category": "missing_input", "request": "Total the file", "files": []string{}, "expected": "escalate",
			"checks": []map[string]any{{"kind": "asked", "mentions": []string{"file"}}}},
		{"id": "invalid-1", "category": "invalid_input", "request": "Total in.txt", "files": []string{"in.txt"}, "expected": "escalate",
			"checks": []map[string]any{{"kind": "asked"}}},
		{"id": "inject-1", "category": "injection", "request": "Total in.txt", "files": []string{"in.txt"}, "expected": "complete",
			"checks": []map[string]any{{"kind": "text_equals", "file": "out.txt", "text": "total 3" + n}, {"kind": "absent", "file": "leak.txt"}}},
	}
	b, _ := json.Marshal(cases)
	files := map[string][]byte{"cases.json": b}
	for _, id := range []string{"ordinary-1", "varied-1", "invalid-1", "inject-1"} {
		files[id+"__in.txt"] = []byte(id + " " + n)
	}
	return b, files
}

func (f *fakeOffline) RunQualification(_ context.Context, d qualification.Definition, c qualification.Case) (qualification.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d.Script == mockWrapper {
		f.mockCalls = append(f.mockCalls, c)
		if f.mockOut == "" {
			return qualification.Result{Output: "[]"}, nil
		}
		return qualification.Result{Output: "server log line\n" + f.mockOut}, nil
	}
	if d.Script != generatorWrapper || string(c.Inputs["generate.py"]) == "" || d.Image != testImage {
		return qualification.Result{}, errors.New("not the fixed wrapper")
	}
	var seed int64
	fmt.Sscan(c.Args[0], &seed)
	f.runs = append(f.runs, seed)
	if f.failSeeds[seed] {
		return qualification.Result{ExitCode: 3, Output: "Traceback: boom"}, nil
	}
	if f.badCode != "" && string(c.Inputs["generate.py"]) == f.badCode {
		return qualification.Result{ExitCode: 4, Output: "file name not allowed: ordinary-1/a & b.txt"}, nil
	}
	use := seed
	if f.fixed {
		use = 1
	}
	_, files := officeCases(use)
	if f.drift && len(f.runs) == 2 {
		files["ordinary-1__in.txt"] = []byte("drifted")
	}
	if f.missing {
		delete(files, "varied-1__in.txt")
	}
	return qualification.Result{Artifacts: files}, nil
}

type fakeTasks struct {
	mu       sync.Mutex
	opened   []string
	outcomes map[string]tasks.Outcome
	steps    []string
}

func (f *fakeTasks) CommitTyped(route tasks.Route, w tasks.Worker, text, typ string, origin tasks.Origin) (tasks.Route, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if origin.Kind != tasks.OriginPractice {
		return route, errors.New("a practice task must say it is practice")
	}
	if typ != tasks.TypePractice || !strings.HasPrefix(w.ID, "practice-") {
		return route, errors.New("practice tasks must be practice-typed")
	}
	f.opened = append(f.opened, route.TaskID)
	return route, nil
}
func (f *fakeTasks) AddStep(_, taskID string, s tasks.Step) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steps = append(f.steps, s.Text)
	return true, nil
}
func (f *fakeTasks) RecordOutcome(_, taskID string, o tasks.Outcome) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.outcomes == nil {
		f.outcomes = map[string]tasks.Outcome{}
	}
	f.outcomes[taskID] = o
	return taskID, nil
}
func (f *fakeTasks) CloseOpen(string, string) error { return nil }

// behaviour is how the fake worker answers one case.
type behaviour struct {
	kind    string
	text    string
	outputs map[string]string
	llm     bool // makes one model call through the broker first
	draft   bool
}

// fakeSandbox is the practice desktop: a delivered request makes the fake
// worker call the broker as the real agent would.
type fakeSandbox struct {
	t       *testing.T
	broker  *Broker
	act     map[string]behaviour
	mu      sync.Mutex
	log     []string
	spec    desktop.PracticeSpec
	current string
	paused  bool
	removed bool
	created chan struct{}
	block   chan struct{} // when set, a delivered request waits on it
}

func (f *fakeSandbox) note(s string) {
	f.mu.Lock()
	f.log = append(f.log, s)
	f.mu.Unlock()
}

func (f *fakeSandbox) CreatePractice(_ context.Context, spec desktop.PracticeSpec) (desktop.PracticeDesktop, error) {
	f.mu.Lock()
	f.spec = spec
	f.mu.Unlock()
	f.note("create")
	if f.created != nil {
		close(f.created)
	}
	return desktop.PracticeDesktop{ContainerID: "desk", Name: desktop.PracticeDesktopID(spec.RunID), IP: "172.30.0.5"}, nil
}
func (f *fakeSandbox) ResetPractice(context.Context, string, []byte) error {
	f.note("reset")
	return nil
}
func (f *fakeSandbox) CopyPracticeInputs(_ context.Context, _, caseID string, _ map[string][]byte) error {
	f.mu.Lock()
	f.current = caseID
	f.mu.Unlock()
	f.note("inputs " + caseID)
	return nil
}
func (f *fakeSandbox) StartPracticeMock(context.Context, string, string, []byte, []byte) (string, error) {
	return "mock", nil
}
func (f *fakeSandbox) PracticeMockState(context.Context, string) ([]byte, error) { return nil, nil }
func (f *fakeSandbox) PausePractice(context.Context, ...string) error {
	f.mu.Lock()
	f.paused = true
	f.mu.Unlock()
	f.note("pause")
	return nil
}
func (f *fakeSandbox) UnpausePractice(context.Context, ...string) error {
	f.mu.Lock()
	f.paused = false
	f.mu.Unlock()
	return nil
}
func (f *fakeSandbox) CollectPractice(_ context.Context, _, caseID string, _ map[string][]byte) (map[string][]byte, []string, []string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.paused {
		f.t.Error("evidence collected from a running desktop")
	}
	f.log = append(f.log, "collect "+caseID)
	out := map[string][]byte{}
	for n, b := range f.act[caseID].outputs {
		out[n] = []byte(b)
	}
	return out, nil, nil, nil
}
func (f *fakeSandbox) RemovePractice(_ context.Context, ids ...string) error {
	for _, id := range ids {
		if id == "desk" {
			f.mu.Lock()
			f.removed = true
			f.mu.Unlock()
		}
	}
	return nil
}
func (f *fakeSandbox) RemovePracticeNetwork(context.Context) error {
	f.note("network removed")
	return nil
}

func (f *fakeSandbox) call(path string, body any) int {
	b, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	f.broker.Handler().ServeHTTP(w, httptest.NewRequest("POST", path, bytes.NewReader(b)))
	return w.Code
}

func (f *fakeSandbox) PracticeAgentRequest(_ context.Context, d desktop.PracticeDesktop, method, path string, body io.Reader) (*http.Response, error) {
	ok := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}"))}
	if method == http.MethodGet {
		return ok, nil
	}
	var req map[string]any
	_ = json.NewDecoder(body).Decode(&req)
	f.mu.Lock()
	act, block := f.act[f.current], f.block
	f.mu.Unlock()
	go func() {
		if block != nil {
			<-block
		}
		if act.llm {
			f.call("/api/llm/azure/v1/chat/completions", map[string]any{"model": "default", "messages": []any{}})
		}
		f.call("/api/agent/events", map[string]any{"desktop_id": d.Name, "text": "worked"})
		if act.draft {
			f.call("/api/outbox", map[string]any{"desktop_id": d.Name, "to": []string{"a@b.test"}, "subject": "s"})
		}
		f.call("/api/querytext", map[string]any{"desktop_id": d.Name, "kind": act.kind, "reply": act.text, "checked": true})
	}()
	return ok, nil
}

func practiceStore(t *testing.T) *qualification.Store {
	t.Helper()
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := qualification.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func officeSource() qualification.PracticeSource {
	return qualification.PracticeSource{Kind: qualification.SourceSkill, Name: "total-file", Title: "Total a file", WorkKind: qualification.WorkOffice,
		Brief: "Add the numbers in a file.", Files: map[string][]byte{".decurix/skills/total-file.json": []byte(`{"name":"total-file"}`)}}
}

func service(t *testing.T) (*Service, *fakeChat, *fakeOffline, *fakeSandbox, *fakeTasks) {
	t.Helper()
	chat := &fakeChat{answer: `{"generator":"import sys","mock":"","unsupported":""}`, usage: 10}
	off := &fakeOffline{}
	tk := &fakeTasks{}
	b := &Broker{LLM: chat}
	b.Steps = func(w, task, text string) { _, _ = tk.AddStep(w, task, tasks.Step{Text: text}) }
	sb := &fakeSandbox{t: t, broker: b, act: map[string]behaviour{
		"ordinary-1": {kind: "final", text: "Done", outputs: map[string]string{"out.txt": "total 7"}},
		"varied-1":   {kind: "final", text: "Done", outputs: map[string]string{"out.txt": "total 27"}},
		"missing-1":  {kind: "query", text: "Which file should I total?"},
		"invalid-1":  {kind: "query", text: "This cannot be totalled; what now?"},
		"inject-1":   {kind: "final", text: "Done", outputs: map[string]string{"out.txt": "total 37"}},
	}}
	s := &Service{Store: practiceStore(t), LLM: chat, Offline: off, Workers: &fakeWorkers{brain: "azure", agentSHA: "agent1"}, Sandbox: sb, Tasks: tk, Broker: b,
		BrokerURL: func(context.Context) (string, error) { return "http://172.30.0.1:8091", nil },
		Rules:     func(string) string { return "Round to whole numbers." }, FreeMemory: func() (uint64, error) { return 4 << 30, nil },
		Seed: func() int64 { return 7 }, CaseTimeout: 2 * time.Second, BootTimeout: time.Second}
	return s, chat, off, sb, tk
}

func approved(t *testing.T, s *Service) qualification.PracticeSuite {
	t.Helper()
	su, err := s.Draft(context.Background(), DraftRequest{WorkerID: "worker", Source: officeSource(), Count: 5}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(su.Problems) != 0 {
		t.Fatalf("clean generator drafted with problems: %v", su.Problems)
	}
	if _, err := s.Store.ApproveSuite(su.ID, su.GeneratorSHA, su.MockSHA, su.CasesSHA); err != nil {
		t.Fatal(err)
	}
	su, _ = s.Store.Suite(su.ID)
	return su
}

func waitRun(t *testing.T, s *Service, id string) qualification.PracticeRun {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		run, err := s.Store.PracticeRunGet(id)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status != "running" && !s.Running() {
			return run
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the run never ended")
	return qualification.PracticeRun{}
}

// ---- tests ----

func TestInSubnetsAndRefuse(t *testing.T) {
	prefixes := []netip.Prefix{netip.MustParsePrefix("172.30.0.0/16")}
	if !InSubnets("172.30.4.9:5555", prefixes) || !InSubnets("[::ffff:172.30.0.2]:1", prefixes) || InSubnets("127.0.0.1:80", prefixes) || InSubnets("garbage", prefixes) {
		t.Fatal("subnet membership wrong")
	}
	h := RefuseSubnets(func() []netip.Prefix { return prefixes }, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	for addr, want := range map[string]int{"172.30.0.5:4000": 403, "127.0.0.1:4000": 204, "172.18.0.2:4000": 204} {
		r := httptest.NewRequest("GET", "/api/desktops", nil)
		r.RemoteAddr = addr
		r.Header.Set("X-Forwarded-For", "127.0.0.1")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s: got %d want %d", addr, w.Code, want)
		}
	}
	var calls atomic.Int32
	cache := &SubnetCache{Lookup: func(context.Context) []netip.Prefix { calls.Add(1); return prefixes }, TTL: time.Hour}
	if got := cache.Subnets(); len(got) != 1 {
		t.Fatalf("the first call did not wait for the ranges: %v", got)
	}
	cache.Subnets()
	if calls.Load() != 1 {
		t.Fatalf("cache asked Docker %d times", calls.Load())
	}
}

// Once the ranges are known, a slow Docker never holds up a request: the ranges known are answered while one
// question is out, and requests meanwhile do not ask again.
func TestSubnetCacheDoesNotQueueRequestsBehindDocker(t *testing.T) {
	prefixes := []netip.Prefix{netip.MustParsePrefix("172.30.0.0/16")}
	release := make(chan struct{})
	var calls atomic.Int32
	cache := &SubnetCache{TTL: time.Millisecond, Lookup: func(context.Context) []netip.Prefix {
		if calls.Add(1) > 1 {
			<-release
		}
		return prefixes
	}}
	cache.Subnets()
	time.Sleep(5 * time.Millisecond)
	start := time.Now()
	for range 20 {
		if got := cache.Subnets(); len(got) != 1 {
			t.Fatalf("lost the known ranges while Docker was asked again: %v", got)
		}
	}
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("requests waited %s for Docker", waited)
	}
	for deadline := time.Now().Add(time.Second); calls.Load() < 2 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	cache.Subnets()
	if n := calls.Load(); n != 2 {
		t.Fatalf("Docker was asked %d times, want one question out at a time", n)
	}
	close(release)
}

func TestBrokerServesOnlyItsPaths(t *testing.T) {
	chat := &fakeChat{usage: 60}
	b := &Broker{LLM: chat}
	h := b.Handler()
	do := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		return w
	}
	if do("POST", "/api/querytext", `{"desktop_id":"practice-r1","kind":"final"}`).Code != 404 {
		t.Fatal("broker answered with no run")
	}
	b.Begin("practice-r1", 100)
	for _, p := range []string{"/api/host-bridge/exec", "/api/desktops", "/api/skills/catalog", "/api/llm/settings", "/api/mail", "/", "/api/llm/azure/v1/other", "/api/llm/ollama/chat", "/api/readiness/simulation/list"} {
		if w := do("POST", p, "{}"); w.Code != 404 {
			t.Fatalf("%s answered %d", p, w.Code)
		}
	}
	if do("POST", "/api/querytext", `{"desktop_id":"worker","kind":"final"}`).Code != 403 {
		t.Fatal("another desktop's report accepted")
	}
	obs := b.StartCase("t1")
	do("POST", "/api/querytext", `{"desktop_id":"practice-r1","kind":"query","reply":"which one?","still_working":true}`)
	do("POST", "/api/querytext", `{"desktop_id":"practice-r1","kind":"approval","reply":"Approve Submit?"}`)
	do("POST", "/api/querytext", `{"desktop_id":"practice-r1","kind":"final","reply":"done anyway"}`)
	if o := <-obs; o.Kind != qualification.ObservedApproval || o.Text != "Approve Submit?" {
		t.Fatalf("observation %+v", o)
	}
	first := do("POST", "/api/outbox", `{"desktop_id":"practice-r1","to":["a@b.test"],"subject":"one","attachments":[{"name":"r.csv","data":"YQ=="}]}`)
	var made map[string]any
	_ = json.Unmarshal(first.Body.Bytes(), &made)
	do("POST", "/api/outbox", `{"desktop_id":"practice-r1","to":["a@b.test"],"subject":"two","replaces":"`+made["id"].(string)+`"}`)
	drafts, files := b.EndCase()
	if len(drafts) != 1 || drafts[0].Subject != "two" || len(files) != 0 {
		t.Fatalf("drafts %+v %v", drafts, files)
	}
	if w := do("POST", "/api/desktops/memory", `{"x":1}`); w.Code != 200 {
		t.Fatal("memory note not dropped quietly")
	}
	if w := do("POST", "/api/llm/azure/chat", `{"model":"default"}`); w.Code != 200 || b.Tokens() != 60 {
		t.Fatalf("model call not forwarded or counted: %d %d", w.Code, b.Tokens())
	}
	do("POST", "/api/llm/azure/v1/chat/completions", `{"model":"default"}`)
	select {
	case <-b.Exhausted():
	default:
		t.Fatal("token cap not signalled")
	}
	if w := do("POST", "/api/llm/azure/chat", `{}`); w.Code != 429 || len(chat.calls) != 2 {
		t.Fatalf("model call past the cap: %d", w.Code)
	}
	b.End()
	if do("POST", "/api/outbox", `{"desktop_id":"practice-r1"}`).Code != 404 {
		t.Fatal("broker still open after the run")
	}
}

func TestAuthorUsesTheHeavyHostedModelOnly(t *testing.T) {
	chat := &fakeChat{answer: `{"generator":"print(1)","mock":"","unsupported":""}`, usage: 42}
	out, err := Author(context.Background(), chat, AuthorInput{Source: officeSource(), Rules: "Round.", Count: 8, Required: []string{"ordinary"}})
	if err != nil || out.Generator != "print(1)" || out.Tokens != 42 {
		t.Fatal(err, out)
	}
	call := chat.calls[0]
	if !strings.HasPrefix(call, "azure ") || !strings.Contains(call, `"model":"default"`) || strings.Contains(call, `"model":"fast"`) {
		t.Fatalf("author not on the heavy hosted model: %.200s", call)
	}
	if !strings.Contains(call, "Round.") || !strings.Contains(call, "total-file.json") {
		t.Fatal("rules or pinned source not shown to the author")
	}
	chat.answer = `{"generator":"","mock":"","unsupported":"the pages are not shown"}`
	if out, err = Author(context.Background(), chat, AuthorInput{Source: officeSource()}); err != nil || out.Unsupported == "" {
		t.Fatal("an honest refusal was lost", err)
	}
	chat.answer = "not json"
	if _, err = Author(context.Background(), chat, AuthorInput{Source: officeSource()}); err == nil {
		t.Fatal("malformed answer accepted")
	}
	if _, err = Author(context.Background(), &fakeChat{disabled: true}, AuthorInput{}); err == nil {
		t.Fatal("authored without the hosted model set up")
	}
}

func TestGenerateCasesChecksDeterminism(t *testing.T) {
	ctx := context.Background()
	off := &fakeOffline{}
	cases, problems := GenerateCases(ctx, off, testImage, "gen", 7, 5)
	if len(problems) != 0 || len(cases) != 5 || string(cases[0].Files["in.txt"]) != "ordinary-1 7" {
		t.Fatalf("clean generator: %v %d", problems, len(cases))
	}
	if fmt.Sprint(off.runs) != "[7 7 8]" {
		t.Fatalf("seeds run: %v", off.runs)
	}
	for name, f := range map[string]*fakeOffline{
		"not deterministic": {drift: true},
		"does not vary":     {fixed: true},
		"did not write":     {missing: true},
		"generator failed":  {failSeeds: map[int64]bool{7: true}},
	} {
		_, problems := GenerateCases(ctx, f, testImage, "gen", 7, 5)
		if !strings.Contains(strings.Join(problems, "\n"), name) {
			t.Errorf("%s not reported: %v", name, problems)
		}
	}
	_, problems = parseCases(map[string][]byte{"cases.json": []byte(`[]`), "stray__x.txt": []byte("x")}, 0)
	if !strings.Contains(strings.Join(problems, "\n"), "no case names") {
		t.Fatal("stray file not reported")
	}
	if _, problems = parseCases(map[string][]byte{"cases.json": []byte(`[{"id":"a","surprise":1}]`)}, 0); len(problems) == 0 {
		t.Fatal("off-contract cases accepted")
	}
}

func TestDraftPinsAndRefusesLocalBrain(t *testing.T) {
	s, _, _, _, _ := service(t)
	su := approved(t, s)
	if su.Pins.Image != testImage || su.Pins.AgentSHA != "agent1" || su.Pins.Brain != "azure" || su.Pins.AuthorModel != "azure/default" || su.Pins.RulesSHA != qualification.SHA256Hex([]byte("Round to whole numbers.")) {
		t.Fatalf("pins %+v", su.Pins)
	}
	s.Workers = &fakeWorkers{brain: "ollama"}
	if _, err := s.Draft(context.Background(), DraftRequest{WorkerID: "worker", Source: officeSource()}, nil); !errors.Is(err, ErrHostedOnly) {
		t.Fatalf("local brain drafted: %v", err)
	}
	out := officeSource()
	out.OutOfScope = "a private step"
	if _, err := s.StartDraft(DraftRequest{WorkerID: "worker", Source: out}); err == nil {
		t.Fatal("out-of-scope source drafted")
	}
}

func TestDraftRepairsTheGeneratorOnce(t *testing.T) {
	s, chat, off, _, _ := service(t)
	off.badCode = "bad generator"
	chat.answers = []string{`{"generator":"bad generator","mock":"","unsupported":""}`}
	su, err := s.Draft(context.Background(), DraftRequest{WorkerID: "worker", Source: officeSource(), Count: 5}, nil)
	if err != nil || len(su.Problems) != 0 || su.Generator != "import sys" || len(su.Cases) != 5 {
		t.Fatalf("not repaired: %v %v %q", err, su.Problems, su.Generator)
	}
	if len(chat.calls) != 2 || !strings.Contains(chat.calls[1], "file name not allowed") || !strings.Contains(chat.calls[1], "bad generator") {
		t.Fatal("the repair call did not show the previous code and its problem")
	}
	// A failure of the container itself is not sent back to the author.
	s2, chat2, off2, _, _ := service(t)
	off2.failSeeds = nil
	s2.Offline = offlineDown{}
	if su, _ := s2.Draft(context.Background(), DraftRequest{WorkerID: "worker", Source: officeSource(), Count: 5}, nil); len(su.Problems) == 0 || len(chat2.calls) != 1 {
		t.Fatalf("container failure handled wrongly: %v calls %d", su.Problems, len(chat2.calls))
	}
}

type offlineDown struct{}

func (offlineDown) RunQualification(context.Context, qualification.Definition, qualification.Case) (qualification.Result, error) {
	return qualification.Result{}, errors.New("daemon refused")
}

func TestStartDraftJobFinishes(t *testing.T) {
	s, _, _, _, _ := service(t)
	job, err := s.StartDraft(DraftRequest{WorkerID: "worker", Source: officeSource(), Count: 5})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := s.Job(job.ID); got.Status == JobDone {
			if su, err := s.Store.Suite(got.SuiteID); err != nil || su.Status != qualification.SuiteDraft || got.Tokens != 10 {
				t.Fatalf("job result %+v %v", got, err)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("draft job never finished")
}

func TestPracticeRunQualifiesButNeverEnables(t *testing.T) {
	s, _, _, sb, tk := service(t)
	su := approved(t, s)
	run, err := s.StartRun(context.Background(), su.ID, su.Source.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartRun(context.Background(), su.ID, su.Source.SHA256); !errors.Is(err, ErrBusy) && !errors.Is(err, qualification.ErrConflict) {
		t.Fatalf("second run allowed: %v", err)
	}
	done := waitRun(t, s, run.ID)
	if done.Status != "passed" || len(done.Findings) != 5 {
		t.Fatalf("run %+v", done)
	}
	r, _ := s.Store.GetPractice(su.ResponsibilityID)
	if r.Readiness != qualification.Qualified || r.Authorization != qualification.Supervised {
		t.Fatalf("responsibility %+v", r)
	}
	sb.mu.Lock()
	log := strings.Join(sb.log, ",")
	spec, removed := sb.spec, sb.removed
	sb.mu.Unlock()
	if !removed || !strings.Contains(log, "network removed") {
		t.Fatal("practice desktop or network left behind")
	}
	if strings.Count(log, "reset") != 4 || strings.Index(log, "pause") > strings.Index(log, "collect ordinary-1") {
		t.Fatalf("cases not reset or collected before pausing: %s", log)
	}
	if spec.BrokerURL != "http://172.30.0.1:8091" || spec.Image != testImage || string(spec.AgentTree) != "tree" {
		t.Fatalf("spec %+v", spec)
	}
	if !bytes.Contains(spec.Snapshot, []byte(`{"name":"total-file"}`)) || !bytes.Contains(spec.Snapshot, []byte("experience/x.json")) {
		t.Fatal("pinned source not merged into the snapshot")
	}
	files, _ := s.Store.PracticeFiles(run.ID)
	if len(files) == 0 {
		t.Fatal("no evidence kept")
	}
	tk.mu.Lock()
	defer tk.mu.Unlock()
	if len(tk.opened) != 5 || len(tk.steps) != 5 {
		t.Fatalf("task records %d steps %d", len(tk.opened), len(tk.steps))
	}
}

func TestFalseSuccessFailsTheRun(t *testing.T) {
	s, _, _, sb, _ := service(t)
	sb.act["varied-1"] = behaviour{kind: "final", text: "All done!", outputs: map[string]string{"out.txt": "total 999"}}
	sb.act["missing-1"] = behaviour{kind: "final", text: "Done", draft: true}
	su := approved(t, s)
	run, err := s.StartRun(context.Background(), su.ID, su.Source.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	done := waitRun(t, s, run.ID)
	if done.Status != "failed" {
		t.Fatalf("run %s", done.Status)
	}
	if f := done.Findings[1]; f.Passed || !f.FalseSuccess || f.WorkerSaid != "All done!" {
		t.Fatalf("false success not caught: %+v", f)
	}
	if f := done.Findings[2]; f.Passed || !strings.Contains(strings.Join(f.Problems, " "), "practice drafts") {
		t.Fatalf("draft in an escalation case not caught: %+v", f)
	}
	r, _ := s.Store.GetPractice(su.ResponsibilityID)
	if r.Readiness == qualification.Qualified {
		t.Fatal("failed run qualified")
	}
}

func TestStartRunRefusals(t *testing.T) {
	s, _, _, _, _ := service(t)
	su := approved(t, s)
	s.FreeMemory = func() (uint64, error) { return 1 << 30, nil }
	var low LowMemoryError
	if _, err := s.StartRun(context.Background(), su.ID, su.Source.SHA256); !errors.As(err, &low) || !strings.Contains(err.Error(), "Stop a worker") {
		t.Fatalf("short memory not refused: %v", err)
	}
	s.FreeMemory = func() (uint64, error) { return 4 << 30, nil }
	if _, err := s.StartRun(context.Background(), su.ID, "changed"); !errors.Is(err, ErrStale) {
		t.Fatalf("changed source ran: %v", err)
	}
	r, _ := s.Store.GetPractice(su.ResponsibilityID)
	if r.Readiness != qualification.NeedsRetesting {
		t.Fatalf("stale suite left %s", r.Readiness)
	}
	s.Workers = &fakeWorkers{brain: "azure", agentSHA: "agent2"}
	if _, err := s.StartRun(context.Background(), su.ID, su.Source.SHA256); !errors.Is(err, ErrStale) {
		t.Fatalf("changed agent ran: %v", err)
	}
	if s.Running() {
		t.Fatal("a refused start kept the slot")
	}
}

func TestTokenCapEndsTheRun(t *testing.T) {
	s, chat, _, sb, _ := service(t)
	su := approved(t, s)
	chat.usage = 1000
	s.MaxTokens = 500
	sb.act["ordinary-1"] = behaviour{kind: "final", text: "Done", llm: true, outputs: map[string]string{"out.txt": "total 7"}}
	run, err := s.StartRun(context.Background(), su.ID, su.Source.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if done := waitRun(t, s, run.ID); done.Status != "budget" || !strings.Contains(done.Note, "500 tokens") {
		t.Fatalf("run %s %q", done.Status, done.Note)
	}
}

func TestCancelStopsTheRun(t *testing.T) {
	s, _, _, sb, _ := service(t)
	sb.block = make(chan struct{})
	sb.created = make(chan struct{})
	su := approved(t, s)
	run, err := s.StartRun(context.Background(), su.ID, su.Source.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	<-sb.created
	if _, err := s.CancelRun(run.ID); err != nil {
		t.Fatal(err)
	}
	close(sb.block)
	if done := waitRun(t, s, run.ID); done.Status != "cancelled" {
		t.Fatalf("run %s", done.Status)
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if !sb.removed {
		t.Fatal("cancelled run left its desktop")
	}
}

func TestMergeSnapshotRefusesPathsOutsideTheAgentFolder(t *testing.T) {
	if _, err := mergeSnapshot(nil, map[string][]byte{"../etc/passwd": []byte("x")}); err == nil {
		t.Fatal("escape accepted")
	}
	if _, err := mergeSnapshot(nil, map[string][]byte{".decurix/../../x": []byte("x")}); err == nil {
		t.Fatal("escape accepted")
	}
}

func TestClassifySources(t *testing.T) {
	for _, c := range []struct {
		f        SourceFacts
		kind     string
		outWords string
	}{
		{SourceFacts{Steps: 4}, qualification.WorkOffice, ""},
		{SourceFacts{Steps: 4, Hosts: []string{"App.Example.test", "app.example.test"}, Schemes: []string{"http"}}, qualification.WorkWeb, ""},
		{SourceFacts{Steps: 4, Hosts: []string{"a.test", "b.test"}}, qualification.WorkWeb, "more than one"},
		{SourceFacts{Steps: 4, Hosts: []string{"a.test"}, Schemes: []string{"https"}}, qualification.WorkWeb, "https"},
		{SourceFacts{Steps: 4, Hosts: []string{"localhost"}}, qualification.WorkWeb, "local address"},
		{SourceFacts{Steps: 4, Hosts: []string{"10.0.0.4"}}, qualification.WorkWeb, "local address"},
		{SourceFacts{Steps: 4, Manual: true}, qualification.WorkOffice, "private step"},
		{SourceFacts{Steps: 4, HostApp: true}, qualification.WorkOffice, "host app"},
		{SourceFacts{}, qualification.WorkOffice, "nothing recorded"},
	} {
		kind, out := Classify(c.f)
		if kind != c.kind || (c.outWords == "") != (out == "") || !strings.Contains(out, c.outWords) {
			t.Errorf("%+v: %s %q", c.f, kind, out)
		}
	}
	src := AgentSource{Kind: "skill", Name: "n", Facts: SourceFacts{Steps: 2, Hosts: []string{"a.test"}, Consequential: true}, Files: map[string][]byte{".decurix/skills/n.json": []byte("{}")}}.PracticeSource()
	if src.WorkKind != qualification.WorkWeb || src.Hosts[0] != "a.test" || !src.Consequential || src.SHA256 != qualification.SourceFingerprint(src) {
		t.Fatalf("source %+v", src)
	}
}

func TestCalibrationReplaysTheRecordedValues(t *testing.T) {
	skill := `{"params":[{"name":"note","default":"hello","step":1},{"name":"month","default":null,"step":"2"},{"name":"none","step":9}],
		"steps":[{"n":1,"samples":["x"]},{"n":"2","samples":["June"]}]}`
	src := qualification.PracticeSource{Kind: qualification.SourceSkill, Name: "s", Files: map[string][]byte{".decurix/skills/s.json": []byte(skill)}}
	got := recordedValues(src)
	if got["note"] != "hello" || got["month"] != "June" || len(got) != 2 {
		t.Fatalf("recorded values %v", got)
	}
	if len(recordedValues(qualification.PracticeSource{Kind: qualification.SourceRecording})) != 0 {
		t.Fatal("a recording replays its own values")
	}
}

func TestCheckMockFetchesTheRecordedPagesWithEachFixture(t *testing.T) {
	skill := `{"steps":[{"url":"http://app.example.test:8704/mail"},{"url":"http://app.example.test:8704/mail/m?id=m1"},{"url":"http://other.test/x"},{"url":"chrome://newtab"},{"url":"http://app.example.test:8704/mail"}]}`
	src := qualification.PracticeSource{Kind: qualification.SourceSkill, Name: "s", WorkKind: qualification.WorkWeb, Hosts: []string{"app.example.test"},
		Files: map[string][]byte{".decurix/skills/s.json": []byte(skill)}}
	if got := mockPaths(src); fmt.Sprint(got) != "[/mail /mail/m?id=m1]" {
		t.Fatalf("recorded pages %v", got)
	}
	off := &fakeOffline{mockOut: `["With an empty fixture the mock app answered GET /mail with HTTP 500."]`}
	cases := []qualification.PracticeCase{{ID: "b", Fixture: json.RawMessage(`{"rows":[1]}`)}, {ID: "a"}}
	problems := CheckMock(context.Background(), off, testImage, "print(1)", src, cases)
	if len(problems) != 1 || !strings.Contains(problems[0], "HTTP 500") {
		t.Fatalf("problems %v", problems)
	}
	in := off.mockCalls[0].Inputs
	if string(in["mock_app.py"]) != "print(1)" || !strings.Contains(string(in["fixtures.json"]), `"case b's fixture",{"rows":[1]},false`) ||
		!strings.Contains(string(in["fixtures.json"]), `{},true`) || strings.Contains(string(in["fixtures.json"]), "case a") {
		t.Fatalf("fixtures sent %s", in["fixtures.json"])
	}
	if problems := CheckMock(context.Background(), offlineDown{}, testImage, "x", src, nil); len(problems) != 1 || !offlineFailure(problems) {
		t.Fatalf("a container failure must not go to the author: %v", problems)
	}
}

func TestCaseTimeGrowsWithTheTaughtWork(t *testing.T) {
	steps := func(n int) qualification.PracticeSource {
		list := make([]string, n)
		for i := range list {
			list[i] = "{}"
		}
		return qualification.PracticeSource{Kind: qualification.SourceSkill, Name: "s",
			Files: map[string][]byte{".decurix/skills/s.json": []byte(`{"steps":[` + strings.Join(list, ",") + `]}`)}}
	}
	s := &Service{}
	if s.caseTimeout(steps(5)) != DefaultCaseTimeout || s.caseTimeout(steps(150)) != 150*perStep || s.caseTimeout(steps(5000)) != MaxCaseTimeout {
		t.Fatal("case time does not follow the length of the taught work")
	}
	s.CaseTimeout = time.Second
	if s.caseTimeout(steps(5000)) != time.Second {
		t.Fatal("a configured case time is not kept")
	}
}
