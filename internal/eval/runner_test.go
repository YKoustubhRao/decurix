package eval

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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/llm"
	"github.com/YKoustubhRao/decurix/internal/llmrec"
	"github.com/YKoustubhRao/decurix/internal/practice"
	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// ---- a suite of three small cases ----

func runnerSuite(t *testing.T, extra ...map[string]any) []byte {
	t.Helper()
	ids := []string{"web-save", "ask-first", "office"}
	files := map[string][]byte{
		"mocks/app/mock_app.py":        []byte("print('mock')\n"),
		"cases/web-save/fixture.json":  []byte(`{"collections": {"bills": [{"id": 1, "number": "OLD-1"}]}}`),
		"cases/ask-first/fixture.json": []byte(`{"collections": {"bills": []}}`),
		"cases/office/files/in.txt":    []byte("1\n2\n"),
		"cases/web-save/case.json": mustJSON(t, map[string]any{"schema": CaseSchema, "id": "web-save", "category": "ordinary", "kind": "web_form",
			"request": "Save bill B-1.", "start": map[string]any{"files": []string{}, "mock": "app", "fixture": "fixture.json"}, "facts": []any{},
			"expected": map[string]any{"outcome": "complete", "graders": []any{map[string]any{"kind": "mock_records", "collection": "bills", "exact_count": true,
				"records": []any{map[string]any{"number": map[string]string{"id": "B-1"}}}}}},
			"oracle": []any{"curl -d number=B-1 x"}}),
		"cases/ask-first/case.json": mustJSON(t, map[string]any{"schema": CaseSchema, "id": "ask-first", "category": "missing_input", "kind": "doc_to_record",
			"request": "Save bill B-2.", "start": map[string]any{"files": []string{}, "mock": "app", "fixture": "fixture.json"},
			"facts": []any{map[string]any{"id": "centre", "about": "which cost centre", "answer": "Charge it to Birch.", "required": true},
				map[string]any{"id": "colour", "about": "the colour of the folder", "answer": "Blue.", "required": false}},
			"expected": map[string]any{"outcome": "complete", "graders": []any{
				map[string]any{"kind": "mock_records", "collection": "bills", "exact_count": true, "records": []any{map[string]any{"number": "B-2", "centre": "Birch"}}},
				map[string]any{"kind": "asked", "facts": []string{"centre"}},
				map[string]any{"kind": "absent", "collection": "bills", "when": "asked"}}},
			"oracle": []any{"curl -d centre=Birch x"}}),
		"cases/office/case.json": mustJSON(t, map[string]any{"schema": CaseSchema, "id": "office", "category": "ordinary", "kind": "office",
			"request": "Total in.txt into out.txt.", "start": map[string]any{"files": []string{"in.txt"}}, "facts": []any{},
			"expected": map[string]any{"outcome": "complete", "graders": []any{map[string]any{"kind": "file_text_equals", "file": "out.txt", "text": "3\n"}}},
			"oracle":   []any{"echo 3 > out.txt", map[string]any{"answer": "Totalled."}}}),
	}
	for _, e := range extra {
		id := e["id"].(string)
		ids = append(ids, id)
		files["cases/"+id+"/case.json"] = mustJSON(t, e)
	}
	files["suite.json"] = mustJSON(t, map[string]any{"schema": SuiteSchema, "name": "runner-test", "description": "",
		"mocks": map[string]any{"app": map[string]any{"hosts": []string{"app.example.test"}}}, "budget": map[string]any{"max_tokens": 5000, "max_minutes": 5},
		"cases": ids})
	return tarFrom(t, files, nil)
}

func silentCase(id string, minutes float64) map[string]any {
	return map[string]any{"schema": CaseSchema, "id": id, "category": "ordinary", "kind": "office", "request": "Do " + id + ".",
		"start": map[string]any{"files": []string{}}, "facts": []any{}, "budget": map[string]any{"max_minutes": minutes},
		"expected": map[string]any{"outcome": "complete", "graders": []any{map[string]any{"kind": "final_text_number", "number": "1"}}},
		"oracle":   []any{map[string]any{"answer": "1"}}}
}

// ---- fakes ----

type fakeHub struct {
	mu          sync.Mutex
	deployments map[string]string // provider/label -> deployment
	expert      string            // the expert's reply content
	expertCalls [][]byte
	workerCalls int
	switchAfter int // after this many worker calls the default deployment changes (0: never)
	endpoint    string
	endpoints   map[string]string
	// throttled lists worker calls (1-based) the upstream answers 429, naming no model.
	throttled map[int]bool
	// served is the model string each worker answer reports ("" reports the deployment's own name).
	served string
}

func newHub() *fakeHub {
	return &fakeHub{deployments: map[string]string{"azure/default": "big-1", "azure/fast": "small-1"}, expert: `{"fact": "centre"}`}
}

// Configured is azure, and any other provider the hub has a deployment for.
func (h *fakeHub) Configured(p string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if p == "azure" {
		return true
	}
	_, ok := h.deployments[p+"/default"]
	return ok
}

// Status says the endpoint each provider is reached at: endpoint (azure) or endpoints[provider], else a fixed one.
func (h *fakeHub) Status() map[string]llm.ProviderStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]llm.ProviderStatus{}
	for _, p := range []string{"azure", "claude"} {
		ep := h.endpoints[p]
		if p == "azure" && h.endpoint != "" {
			ep = h.endpoint
		}
		if ep == "" {
			ep = "https://" + p + ".models.example.test"
		}
		out[p] = llm.ProviderStatus{Configured: true, Endpoint: ep}
	}
	return out
}

func (h *fakeHub) Resolve(provider, asked string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if asked == "" {
		asked = "default"
	}
	d, ok := h.deployments[provider+"/"+asked]
	if !ok {
		return "", llm.ErrNotConfigured
	}
	return d, nil
}

func (h *fakeHub) Chat(ctx context.Context, provider string, body []byte) (int, []byte, http.Header, error) {
	r, err := h.ChatResult(ctx, provider, body)
	return r.Status, r.Body, r.Retry, err
}

func (h *fakeHub) ChatResult(_ context.Context, provider string, body []byte) (llm.ChatResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if strings.Contains(string(body), "Never answer the question yourself") {
		h.expertCalls = append(h.expertCalls, append([]byte{}, body...))
		content, _ := json.Marshal(h.expert)
		return llm.ChatResult{Status: 200, Deployment: h.deployments["azure/default"],
			Body: []byte(fmt.Sprintf(`{"choices":[{"message":{"content":%s}}],"usage":{"prompt_tokens":20,"completion_tokens":5}}`, content))}, nil
	}
	h.workerCalls++
	if h.switchAfter > 0 && h.workerCalls > h.switchAfter {
		h.deployments["azure/default"] = "big-2"
	}
	if h.throttled[h.workerCalls] {
		return llm.ChatResult{Status: 429, Deployment: h.deployments[provider+"/default"],
			Body: []byte(`{"error":{"code":"429","message":"Rate limit is exceeded. Try again later."}}`)}, nil
	}
	var req struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
	}
	_ = json.Unmarshal(body, &req)
	label := req.Model
	if label == "" {
		label = "default"
	}
	used := 100
	if req.MaxTokens > 0 {
		used = req.MaxTokens
	}
	dep := h.deployments[provider+"/"+label]
	served := h.served
	if served == "" {
		served = dep + "-2026-01-01"
	}
	return llm.ChatResult{Status: 200, Deployment: dep,
		Body: []byte(fmt.Sprintf(`{"model":%q,"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":%d,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":40}}}`, served, used))}, nil
}

// worker is the fake agent's behaviour: what it does with each /request of a case.
type worker func(fs *fakeSandbox, caseID string, content string, n int)

type fakeSandbox struct {
	t      *testing.T
	broker *practice.Broker
	work   worker

	mu        sync.Mutex
	desktopID string
	specs     []desktop.PracticeSpec
	live      map[string]bool // container and mock ids not yet removed
	mocks     map[string]map[string][]map[string]any
	caseID    string
	mockID    string
	outputs   map[string]map[string][]byte
	requests  []map[string]string
	perCase   map[string]int
	execs     [][]string
	execExit  func(argv []string) (int, []byte)
	paused    map[string]bool
	resets    int
	// home is the files in the desktop's home that outlive a case unless the reset clears them.
	home     map[string]bool
	onCreate func(n int)
	// hold, when set, keeps every CreatePractice waiting until it is closed; created says one is waiting.
	hold    chan struct{}
	created chan struct{}
	// failCollect makes every collection of a case's files fail.
	failCollect bool
	blockReq    chan struct{}
	seenWork    chan struct{}
}

func newSandbox(t *testing.T, b *practice.Broker, w worker) *fakeSandbox {
	return &fakeSandbox{t: t, broker: b, work: w, live: map[string]bool{}, mocks: map[string]map[string][]map[string]any{},
		outputs: map[string]map[string][]byte{}, perCase: map[string]int{}, paused: map[string]bool{}, home: map[string]bool{}}
}

func (f *fakeSandbox) CreatePractice(_ context.Context, spec desktop.PracticeSpec) (desktop.PracticeDesktop, error) {
	if f.hold != nil {
		select {
		case f.created <- struct{}{}:
		default: // the test has already seen one
		}
		<-f.hold
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.specs = append(f.specs, spec)
	f.desktopID = desktop.PracticeDesktopID(spec.RunID)
	f.home = map[string]bool{}
	if f.onCreate != nil {
		f.onCreate(len(f.specs))
	}
	id := "c-" + spec.RunID
	f.live[id] = true
	return desktop.PracticeDesktop{ContainerID: id, Name: f.desktopID, IP: "172.30.0.5"}, nil
}

// ResetPractice is a stand-in: it drops from the fake home the paths the real reset is given
// (desktop.PracticeResetPaths). What the real reset does on a desktop is TestPracticeResetLive's to prove.
func (f *fakeSandbox) ResetPractice(context.Context, string, []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resets++
	for file := range f.home {
		for _, root := range desktop.PracticeResetPaths() {
			if file == root || strings.HasPrefix(file, root+"/") {
				delete(f.home, file)
			}
		}
	}
	return nil
}

// learned is where the agent keeps a method after a checked done (methods.py: ~/.decurix/methods).
const learned = "/home/headless/.decurix/methods/enter-a-bill.json"

func (f *fakeSandbox) knows(file string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.home[file]
}

func (f *fakeSandbox) keep(file string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.home[file] = true
}
func (f *fakeSandbox) CopyPracticeInputs(_ context.Context, _ string, caseID string, _ map[string][]byte) error {
	f.mu.Lock()
	f.caseID = caseID
	f.outputs[caseID] = map[string][]byte{}
	f.perCase[caseID] = 0
	f.mu.Unlock()
	return nil
}
func (f *fakeSandbox) StartPracticeMock(_ context.Context, _, _ string, _, fixture []byte) (string, error) {
	var fx struct {
		Collections map[string][]map[string]any `json:"collections"`
	}
	_ = json.Unmarshal(fixture, &fx)
	f.mu.Lock()
	defer f.mu.Unlock()
	id := fmt.Sprintf("m-%d", len(f.mocks)+1)
	if fx.Collections == nil {
		fx.Collections = map[string][]map[string]any{}
	}
	f.mocks[id] = fx.Collections
	f.live[id] = true
	f.mockID = id
	return id, nil
}
func (f *fakeSandbox) PracticeMockState(_ context.Context, id string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return json.Marshal(map[string]any{"collections": f.mocks[id]})
}
func (f *fakeSandbox) PausePractice(_ context.Context, ids ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		f.paused[id] = true
	}
	return nil
}
func (f *fakeSandbox) UnpausePractice(_ context.Context, ids ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		delete(f.paused, id)
	}
	return nil
}
func (f *fakeSandbox) CollectPractice(_ context.Context, id, caseID string, _ map[string][]byte) (map[string][]byte, []string, []string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failCollect {
		return nil, nil, nil, errors.New("the copy out of the container failed")
	}
	if !f.paused[id] {
		f.t.Errorf("collected %s without a pause", id)
	}
	out := map[string][]byte{}
	for k, v := range f.outputs[caseID] {
		out[k] = v
	}
	return out, nil, nil, nil
}
func (f *fakeSandbox) RemovePractice(_ context.Context, ids ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		delete(f.live, id)
	}
	return nil
}
func (f *fakeSandbox) RemovePracticeNetwork(context.Context) error { return nil }
func (f *fakeSandbox) PracticeAgentRequest(ctx context.Context, d desktop.PracticeDesktop, method, path string, body io.Reader) (*http.Response, error) {
	ok := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}"))}
	if method == http.MethodGet {
		return ok, nil
	}
	var req map[string]string
	_ = json.NewDecoder(body).Decode(&req)
	f.mu.Lock()
	f.requests = append(f.requests, req)
	caseID := f.caseID
	f.perCase[caseID]++
	n := f.perCase[caseID]
	block, seen := f.blockReq, f.seenWork
	f.mu.Unlock()
	go func() {
		if seen != nil {
			select {
			case seen <- struct{}{}:
			default:
			}
		}
		if block != nil {
			<-block
			return
		}
		f.work(f, caseID, req["content"], n)
	}()
	return ok, nil
}
func (f *fakeSandbox) ExecPracticeAsWorker(_ context.Context, _ string, argv []string, _ time.Duration) (int, []byte, error) {
	f.mu.Lock()
	f.execs = append(f.execs, argv)
	exit := f.execExit
	f.mu.Unlock()
	if exit != nil {
		code, out := exit(argv)
		return code, out, nil
	}
	return 3, nil, nil
}
func (f *fakeSandbox) DesktopImageID(context.Context, string) (string, error) {
	return "sha256:image1", nil
}

// VerifyGateQuestion takes "gate-signed" as the desktop gate's signature, nothing else.
func (f *fakeSandbox) VerifyGateQuestion(_, requestID, question, sig string) bool {
	return requestID != "" && question != "" && sig == "gate-signed"
}

// ---- what the fake worker does ----

func (f *fakeSandbox) callback(path string, body map[string]any) {
	f.mu.Lock()
	body["desktop_id"] = f.desktopID
	f.mu.Unlock()
	b, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	f.broker.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b)))
}

func (f *fakeSandbox) think(maxTokens int) {
	body := map[string]any{"model": "default", "messages": []any{map[string]string{"role": "user", "content": "x"}}}
	if maxTokens > 0 {
		body["max_tokens"] = maxTokens
	}
	b, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/llm/azure/chat", bytes.NewReader(b))
	r.Header.Set(llmrec.HeaderPurpose, "operator.turn")
	f.broker.Handler().ServeHTTP(w, r)
}

func (f *fakeSandbox) addRecord(coll string, rec map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.mocks[f.mockID]
	rec["id"] = len(m[coll]) + 1
	m[coll] = append(m[coll], rec)
}

func (f *fakeSandbox) save(caseID, name, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outputs[caseID][name] = []byte(text)
}

func (f *fakeSandbox) final(text string, checked bool) {
	f.callback("/api/querytext", map[string]any{"kind": "final", "reply": text, "checked": checked})
}

// diligent does each case right, asking for the cost centre it cannot know.
func diligent(fs *fakeSandbox, caseID, content string, n int) {
	fs.think(0)
	switch caseID {
	case "web-save":
		fs.addRecord("bills", map[string]any{"number": "B-1"})
		fs.final("Saved B-1.", true)
	case "ask-first":
		// A method kept from an earlier case already holds the person's answer: a worker that has one does not ask.
		if n == 1 && !fs.knows(learned) {
			fs.callback("/api/querytext", map[string]any{"kind": "query", "reply": "Which cost centre should B-2 go to?"})
			return
		}
		if strings.Contains(content, "Birch") || fs.knows(learned) {
			fs.addRecord("bills", map[string]any{"number": "B-2", "centre": "Birch"})
			fs.keep(learned)
			fs.final("Saved B-2 to Birch.", true)
			return
		}
		fs.final("I could not tell the cost centre.", false)
	case "office":
		fs.save("office", "out.txt", "3\n")
		fs.final("Totalled.", true)
	}
}

// ---- the service ----

func newService(t *testing.T, w worker) (*Service, *fakeSandbox, *fakeHub, *practice.Service, *recorder) {
	t.Helper()
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "eval.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store, err := NewStore(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	hub := newHub()
	rec := &recorder{}
	b := &practice.Broker{LLM: hub, Recorder: rec}
	sb := newSandbox(t, b, w)
	slot := &practice.Service{}
	s := &Service{Store: store, Sandbox: sb, Broker: b, BrokerURL: func(context.Context) (string, error) { return "http://172.30.0.1:8091", nil },
		Hub: hub, Recorder: rec, Practice: slot, FreeMemory: func() (uint64, error) { return 8 << 30, nil },
		BootTimeout: time.Minute}
	return s, sb, hub, slot, rec
}

// waitDone waits for the run's end as an event (Service.Wait), with no deadline of its own: go test's -timeout ends a
// run that hangs.
func waitDone(t *testing.T, s *Service, id string) Run {
	t.Helper()
	if err := s.Wait(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	run, err := s.Store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func rowsBy(rows []Row, arm, caseID string) []Row {
	var out []Row
	for _, r := range rows {
		if r.Arm == arm && r.CaseID == caseID {
			out = append(out, r)
		}
	}
	return out
}

// ---- tests ----

func TestMatrixTwoArmsTwoRepeatsThreeCases(t *testing.T) {
	s, sb, hub, slot, rec := newService(t, diligent)
	// The first arm waits at its desktop's creation until the test has checked what holds meanwhile.
	sb.hold, sb.created = make(chan struct{}), make(chan struct{}, 1)
	tree := agentTree(t)
	run, err := s.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 2,
		Arms: []Arm{{Name: "default", Kind: ArmModel}, {Name: "fast+expert", Kind: ArmModel, Model: "fast", Expert: true, AgentTree: tree, AgentTreeRev: "abc123"}}})
	if err != nil {
		t.Fatal(err)
	}
	<-sb.created
	// Meanwhile no second eval and no Simulate run starts: the practice slot is held.
	if _, err := s.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 1, Arms: []Arm{{Name: "nothing", Kind: ArmNothing}}}); !errors.Is(err, ErrBusy) {
		t.Fatalf("a second eval started: %v", err)
	}
	if _, err := slot.Reserve(); !errors.Is(err, practice.ErrBusy) {
		t.Fatalf("the practice slot was free during an eval: %v", err)
	}
	close(sb.hold)
	run = waitDone(t, s, run.ID)
	if run.Status != RunDone || strings.Contains(run.Note, "invalid") {
		t.Fatalf("run: %+v", run)
	}
	rows, _ := s.Store.Rows(run.ID)
	if len(rows) != 12 {
		t.Fatalf("%d rows, want 12", len(rows))
	}
	// Everything made was removed, and the slot is free again.
	if len(sb.live) != 0 {
		t.Fatalf("left behind: %v", sb.live)
	}
	release, err := slot.Reserve()
	if err != nil {
		t.Fatalf("the slot was not released: %v", err)
	}
	release()

	for _, r := range rowsBy(rows, "fast+expert", "ask-first") {
		if !r.Passed || r.QRequired != 1 || r.QOther != 0 || r.Observed != OutcomeComplete {
			t.Fatalf("with the expert the question is answered and the case passes: %+v", r)
		}
		var checks rowChecks
		_ = json.Unmarshal(r.Checks, &checks)
		if len(checks.Questions) != 1 || checks.Questions[0].Fact != "centre" || checks.Questions[0].Reply != "Charge it to Birch." {
			t.Fatalf("the question, choice and reply are kept: %s", r.Checks)
		}
	}
	for _, r := range rowsBy(rows, "default", "ask-first") {
		if !r.Passed || r.Observed != OutcomeAsked || r.QRequired != 1 {
			t.Fatalf("without the expert the question ends the case, and asking passes: %+v", r)
		}
	}
	for _, r := range rows {
		if r.CaseID != "ask-first" && !r.Passed {
			t.Fatalf("row failed: %+v", r)
		}
		if r.TaskID == "" || r.TokensIn == 0 || r.TokensCached == 0 {
			t.Fatalf("row lacks its task or usage: %+v", r)
		}
	}
	// Each arm's calls were served by its own deployment: the fast arm asked "default", the broker served "fast".
	for _, r := range rowsBy(rows, "fast+expert", "office") {
		if len(r.Deployments) != 1 || r.Deployments[0] != "small-1" {
			t.Fatalf("fast arm deployments: %v", r.Deployments)
		}
	}
	for _, r := range rowsBy(rows, "default", "office") {
		if len(r.Deployments) != 1 || r.Deployments[0] != "big-1" {
			t.Fatalf("default arm deployments: %v", r.Deployments)
		}
	}
	// The arm's agent tree went into its practice desktop unchanged; the other arm ran the image's.
	if len(sb.specs) != 2 || sb.specs[0].AgentTree != nil || !bytes.Equal(sb.specs[1].AgentTree, tree) {
		t.Fatalf("agent trees: %d specs", len(sb.specs))
	}
	if sb.specs[1].Brain != "azure" || sb.specs[1].Model != "fast" || sb.specs[0].RunID == sb.specs[1].RunID {
		t.Fatalf("specs: %+v", sb.specs[1])
	}
	arms := run.Arms
	if arms[0].Deployment != "big-1" || arms[1].Deployment != "small-1" || arms[1].AgentTreeSHA == "" || arms[0].Image != "sha256:image1" {
		t.Fatalf("pins: %+v", arms)
	}
	// The expert was asked in both arms (it classifies even when it does not answer), and its calls were recorded.
	if len(hub.expertCalls) != 4 {
		t.Fatalf("expert calls: %d", len(hub.expertCalls))
	}
	expertRecorded, practiceRecorded := 0, 0
	for _, c := range rec.calls {
		switch {
		case c.Purpose == ExpertPurpose && c.Caller == llmrec.CallerEval:
			expertRecorded++
		case c.Caller == llmrec.CallerPractice && c.Purpose == "operator.turn" && c.Task != "":
			practiceRecorded++
		}
	}
	if expertRecorded != 4 || practiceRecorded == 0 {
		t.Fatalf("recorded: expert %d, practice %d", expertRecorded, practiceRecorded)
	}
	if sb.resets != 10 {
		t.Fatalf("resets: %d (every case but each arm's first)", sb.resets)
	}
}

// agentTree is a tar rooted at decurix_agent/, as git archive --prefix=decurix_agent/ writes it.
func agentTree(t *testing.T) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "decurix_agent/", Typeflag: tar.TypeDir, Mode: 0o755})
	body := []byte("print('agent')\n")
	_ = tw.WriteHeader(&tar.Header{Name: "decurix_agent/app.py", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
	_, _ = tw.Write(body)
	_ = tw.Close()
	return buf.Bytes()
}

func TestCalibrationArms(t *testing.T) {
	s, sb, _, _, _ := newService(t, diligent)
	sb.execExit = func(argv []string) (int, []byte) {
		if argv[0] == "/bin/sh" {
			return 3, nil // no ledger
		}
		line := argv[len(argv)-1]
		switch {
		case strings.Contains(line, "number=B-1"):
			sb.addRecord("bills", map[string]any{"number": "B-1"})
		case strings.Contains(line, "centre=Birch"):
			sb.addRecord("bills", map[string]any{"number": "B-2", "centre": "Birch"})
		case strings.Contains(line, "echo 3"):
			sb.save("office", "out.txt", "3\n")
		}
		return 0, nil
	}
	run, err := s.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 1, Arms: []Arm{{Name: "oracle", Kind: ArmOracle}, {Name: "nothing", Kind: ArmNothing}}})
	if err != nil {
		t.Fatal(err)
	}
	run = waitDone(t, s, run.ID)
	rows, _ := s.Store.Rows(run.ID)
	for _, r := range rows {
		if r.Arm == "oracle" && !r.Passed {
			t.Fatalf("the oracle fails %s: %+v", r.CaseID, r)
		}
		if r.Arm == "nothing" && (r.Passed || !r.FalseDone) {
			t.Fatalf("a bare done passes %s: %+v", r.CaseID, r)
		}
	}
	// The oracle's lines ran as the worker, in the case's folder, with the task's id.
	var oracle []string
	for _, argv := range sb.execs {
		if argv[0] == "/bin/bash" {
			oracle = argv
		}
	}
	if len(oracle) != 7 || oracle[4] == "" || oracle[5] != desktop.PracticeWorkDir+"/office" || oracle[6] != "echo 3 > out.txt" {
		t.Fatalf("oracle argv: %q", oracle)
	}
	if len(sb.requests) != 0 {
		t.Fatalf("a calibration arm sent the worker a request: %v", sb.requests)
	}
}

func TestATimeoutRowAndABudgetRow(t *testing.T) {
	s, _, _, _, _ := newService(t, func(fs *fakeSandbox, caseID, _ string, _ int) {
		switch caseID {
		case "slow":
			fs.think(0) // and then nothing
		case "costly":
			fs.think(6000) // more than the suite's 5,000-token case budget
		}
	})
	// The budget minute is a real minute: "slow" has 60 ms and does nothing, so its timer is what this tests;
	// "costly" has a minute, and its first call passes its token budget long before that.
	tar := runnerSuite(t, silentCase("slow", 0.001), silentCase("costly", 1))
	run, err := s.Start(context.Background(), MatrixRequest{SuiteTar: tar, Repeats: 1, Cases: []string{"slow", "costly"}, Arms: []Arm{{Name: "default", Kind: ArmModel}}})
	if err != nil {
		t.Fatal(err)
	}
	run = waitDone(t, s, run.ID)
	rows, _ := s.Store.Rows(run.ID)
	if len(rows) != 2 {
		t.Fatalf("rows: %+v", rows)
	}
	if rows[0].CaseID != "slow" || rows[0].Observed != OutcomeTimeout || rows[0].Passed || !containsString(rows[0].Problems, "The case ran out of time.") {
		t.Fatalf("timeout row: %+v", rows[0])
	}
	if rows[1].CaseID != "costly" || rows[1].Observed != OutcomeBudget || rows[1].Passed || rows[1].TokensIn < 5000 {
		t.Fatalf("budget row: %+v", rows[1])
	}
}

func TestAChangedDeploymentMarksTheRowAndTheRunInvalid(t *testing.T) {
	s, _, hub, _, _ := newService(t, diligent)
	hub.switchAfter = 1
	run, err := s.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 1, Cases: []string{"web-save", "office"}, Arms: []Arm{{Name: "default", Kind: ArmModel}}})
	if err != nil {
		t.Fatal(err)
	}
	run = waitDone(t, s, run.ID)
	rows, _ := s.Store.Rows(run.ID)
	if rows[0].Invalid || !rows[1].Invalid || rows[1].Deployments[0] != "big-2" {
		t.Fatalf("rows: %+v", rows)
	}
	if run.Status != RunDone || !strings.Contains(run.Note, "invalid") {
		t.Fatalf("run: %+v", run)
	}
}

func TestCancelMidCaseRemovesTheDesktopAndTheMock(t *testing.T) {
	s, sb, _, slot, _ := newService(t, diligent)
	sb.blockReq = make(chan struct{})
	sb.seenWork = make(chan struct{}, 1)
	run, err := s.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 1, Arms: []Arm{{Name: "default", Kind: ArmModel}}})
	if err != nil {
		t.Fatal(err)
	}
	<-sb.seenWork // go test's -timeout ends a case that never starts
	sb.mu.Lock()
	mid := len(sb.live)
	sb.mu.Unlock()
	if mid != 2 {
		t.Fatalf("a desktop and a mock should be up mid-case: %v", sb.live)
	}
	if _, err := s.Cancel(run.ID); err != nil {
		t.Fatal(err)
	}
	run = waitDone(t, s, run.ID)
	close(sb.blockReq)
	if run.Status != RunCancelled {
		t.Fatalf("run: %+v", run)
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if len(sb.live) != 0 {
		t.Fatalf("left behind after cancel: %v", sb.live)
	}
	release, err := slot.Reserve()
	if err != nil {
		t.Fatalf("the slot was not released: %v", err)
	}
	release()
}

func TestStartRefusesBeforeMakingAnything(t *testing.T) {
	s, sb, _, slot, _ := newService(t, diligent)
	cases := map[string]MatrixRequest{
		"bad suite":    {SuiteTar: []byte("not a tar"), Repeats: 1, Arms: []Arm{{Name: "a", Kind: ArmNothing}}},
		"no arms":      {SuiteTar: runnerSuite(t), Repeats: 1},
		"bad repeats":  {SuiteTar: runnerSuite(t), Repeats: 0, Arms: []Arm{{Name: "a", Kind: ArmNothing}}},
		"bad kind":     {SuiteTar: runnerSuite(t), Repeats: 1, Arms: []Arm{{Name: "a", Kind: "robot"}}},
		"unknown case": {SuiteTar: runnerSuite(t), Repeats: 1, Cases: []string{"nope"}, Arms: []Arm{{Name: "a", Kind: ArmNothing}}},
		"bad tree":     {SuiteTar: runnerSuite(t), Repeats: 1, Arms: []Arm{{Name: "a", Kind: ArmModel, AgentTree: tarFrom(t, map[string][]byte{"etc/passwd": []byte("x")}, nil)}}},
		"claude":       {SuiteTar: runnerSuite(t), Repeats: 1, Arms: []Arm{{Name: "a", Kind: ArmModel, Provider: "claude"}}},
		"same names":   {SuiteTar: runnerSuite(t), Repeats: 1, Arms: []Arm{{Name: "a", Kind: ArmNothing}, {Name: "a", Kind: ArmOracle}}},
	}
	for name, req := range cases {
		if _, err := s.Start(context.Background(), req); err == nil {
			t.Errorf("%s: started", name)
		}
	}
	s.FreeMemory = func() (uint64, error) { return 1 << 30, nil }
	var low practice.LowMemoryError
	if _, err := s.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 1, Arms: []Arm{{Name: "a", Kind: ArmNothing}}}); !errors.As(err, &low) {
		t.Fatalf("short memory: %v", err)
	}
	if len(sb.specs) != 0 {
		t.Fatal("a refused run made a desktop")
	}
	release, err := slot.Reserve()
	if err != nil {
		t.Fatalf("a refused run kept the slot: %v", err)
	}
	release()
	list, _ := s.Store.List(0)
	if len(list) != 0 {
		t.Fatalf("a refused run was stored: %+v", list)
	}
}

func TestARestartMarksARunningRunInterrupted(t *testing.T) {
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "eval.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store, err := NewStore(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(Run{ID: "e1", Arms: []Arm{}, Repeats: 1, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	_ = store.AddRow(Row{RunID: "e1", Arm: "a", Repeat: 1, CaseID: "c", Observed: OutcomeComplete, Passed: true, At: time.Now()})
	if err := store.Create(Run{ID: "e2", Arms: []Arm{}, Repeats: 1}); !errors.Is(err, ErrBusy) {
		t.Fatalf("a second running run was stored: %v", err)
	}
	again, err := NewStore(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	run, _ := again.Get("e1")
	rows, _ := again.Rows("e1")
	if run.Status != RunInterrupted || len(rows) != 1 || !strings.Contains(run.Note, "restarted") {
		t.Fatalf("after a restart: %+v %d rows", run, len(rows))
	}
}

func TestRowsAreOnlyInserted(t *testing.T) {
	for _, file := range []string{"store.go", "runner.go"} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.ToUpper(string(b))
		if strings.Contains(text, "UPDATE EVAL_ROWS") || strings.Contains(text, "DELETE FROM EVAL_ROWS") || strings.Contains(text, "DELETE FROM EVAL_RUNS") {
			t.Fatalf("%s updates or deletes what labels a run (rule 4)", file)
		}
	}
}

// A git archive starts with a pax global header (the commit id); the tree is still rooted at decurix_agent/.
func TestAGitArchiveTreeIsAccepted(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "eb15981"}})
	_ = tw.WriteHeader(&tar.Header{Name: "decurix_agent/", Typeflag: tar.TypeDir, Mode: 0o755})
	body := []byte("x = 1\n")
	_ = tw.WriteHeader(&tar.Header{Name: "decurix_agent/app.py", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
	_, _ = tw.Write(body)
	_ = tw.Close()
	if !treeRooted(buf.Bytes()) {
		t.Fatal("a git archive of the agent tree was refused")
	}
	if treeRooted(tarFrom(t, map[string][]byte{"other/app.py": body}, nil)) {
		t.Fatal("a tree outside decurix_agent/ was accepted")
	}
}
