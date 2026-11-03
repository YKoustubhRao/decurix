package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/practice"
	"github.com/YKoustubhRao/decurix/internal/qualification"
)

type simWorkers struct{}

func (simWorkers) Get(_ context.Context, id string) (desktop.Desktop, error) {
	return desktop.Desktop{ID: id, Harness: "thebot", Brain: "azure", Model: "big"}, nil
}
func (simWorkers) QualificationImage(context.Context, string) (string, error) {
	return "sha256:img", nil
}
func (simWorkers) PracticeAgentTree(context.Context, string) ([]byte, string, error) {
	return nil, "agent", nil
}
func (simWorkers) PracticeSnapshot(context.Context, string) ([]byte, string, error) {
	return nil, "", nil
}

type simChat struct{}

func (simChat) Configured(p string) bool { return p == "azure" }
func (simChat) Chat(context.Context, string, []byte) (int, []byte, http.Header, error) {
	content, _ := json.Marshal(`{"generator":"import sys","mock":"","unsupported":""}`)
	return 200, []byte(`{"choices":[{"message":{"content":` + string(content) + `}}],"usage":{"total_tokens":5}}`), nil, nil
}

// simOffline is the generator in the offline container: one case per category.
type simOffline struct{}

func (simOffline) RunQualification(_ context.Context, _ qualification.Definition, c qualification.Case) (qualification.Result, error) {
	seed := c.Args[0]
	var cases []map[string]any
	files := map[string][]byte{}
	for _, cat := range []string{"ordinary", "varied", "missing_input", "invalid_input", "injection"} {
		id := strings.ReplaceAll(cat, "_", "-")
		entry := map[string]any{"id": id, "category": cat, "request": "Do " + id, "files": []string{"in.txt"}, "expected": qualification.ExpectedFor(cat)}
		switch qualification.ExpectedFor(cat) {
		case "complete":
			entry["checks"] = []map[string]any{{"kind": "text_equals", "file": "out.txt", "text": "ok " + seed}, {"kind": "absent", "file": "leak.txt"}}
		default:
			entry["checks"] = []map[string]any{{"kind": "asked"}}
		}
		cases = append(cases, entry)
		files[id+"__in.txt"] = []byte(id + seed)
	}
	files["cases.json"], _ = json.Marshal(cases)
	return qualification.Result{Artifacts: files}, nil
}

// simAgent answers the worker agent's practice endpoints.
func simAgent(skillBytes *string) func(context.Context, string, string, string, io.Reader) (*http.Response, error) {
	return func(_ context.Context, _, _, path string, _ io.Reader) (*http.Response, error) {
		var body any
		switch path {
		case "/practice-sources":
			body = map[string]any{"sources": []map[string]any{
				{"kind": "skill", "name": "total-file", "title": "Total a file", "facts": map[string]any{"steps": 3, "grade": "ready"}},
				{"kind": "skill", "name": "sign-in", "title": "Sign in", "facts": map[string]any{"steps": 3, "manual": true, "grade": "risky"}},
				{"kind": "skill", "name": "../evil", "facts": map[string]any{"steps": 1}},
			}}
		case "/skills/total-file/practice-source":
			body = map[string]any{"kind": "skill", "name": "total-file", "title": "Total a file", "facts": map[string]any{"steps": 3},
				"files": map[string][]byte{".decurix/skills/total-file.json": []byte(*skillBytes)}}
		case "/skills/sign-in/practice-source":
			body = map[string]any{"kind": "skill", "name": "sign-in", "facts": map[string]any{"steps": 3, "manual": true}}
		default:
			return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("{}"))}, nil
		}
		b, _ := json.Marshal(body)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(b))}, nil
	}
}

type ownerClient struct {
	t      *testing.T
	mux    *http.ServeMux
	cookie *http.Cookie
	csrf   string
}

func (o ownerClient) do(method, path, body string) *httptest.ResponseRecorder {
	o.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.AddCookie(o.cookie)
	r.Header.Set("X-Readiness-CSRF", o.csrf)
	w := httptest.NewRecorder()
	o.mux.ServeHTTP(w, r)
	return w
}

func simulationServer(t *testing.T, memory uint64) (*Server, ownerClient, *practice.Service, *string) {
	t.Helper()
	s := qualificationServer(t)
	skill := `{"name":"total-file","steps":[]}`
	s.agentRequest = simAgent(&skill)
	p := &practice.Service{Store: s.Qualification, LLM: simChat{}, Offline: simOffline{}, Workers: simWorkers{}, Broker: &practice.Broker{},
		Sandbox: nil, BrokerURL: func(context.Context) (string, error) { return "http://172.30.0.1:8091", nil },
		FreeMemory: func() (uint64, error) { return memory, nil }, Seed: func() int64 { return 3 }}
	mux := http.NewServeMux()
	s.RegisterLocalRoutes(mux)
	s.RegisterSimulationRoutes(mux, p)
	w := httptest.NewRecorder()
	s.qualificationSessionHandler(w, httptest.NewRequest("POST", "/api/readiness/session", strings.NewReader(`{"key":"`+s.qualificationAuth.key+`"}`)))
	var session map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &session)
	return s, ownerClient{t: t, mux: mux, cookie: w.Result().Cookies()[0], csrf: session["csrf"]}, p, &skill
}

func TestSimulationRoutesArePrimaryOnlyAndOwnerOnly(t *testing.T) {
	s, owner, _, _ := simulationServer(t, 4<<30)
	gated := http.NewServeMux()
	s.RegisterRoutes(gated)
	paths := []string{"sources", "draft", "approve", "retire", "list", "suites", "run", "cancel", "history", "evidence", "file", "authorize"}
	for _, p := range paths {
		path := "/api/readiness/simulation/" + p
		if _, pattern := gated.Handler(httptest.NewRequest("GET", path, nil)); pattern != "/" {
			t.Fatalf("%s is on the gated listener", path)
		}
		w := httptest.NewRecorder()
		owner.mux.ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader("{}")))
		if w.Code != 401 {
			t.Fatalf("%s without the owner session: %d", path, w.Code)
		}
		r := httptest.NewRequest("POST", path, strings.NewReader("{}"))
		r.AddCookie(owner.cookie)
		w = httptest.NewRecorder()
		owner.mux.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatalf("%s without the CSRF token: %d", path, w.Code)
		}
	}
}

func TestSimulationDraftApproveAndRunRefusals(t *testing.T) {
	_, owner, p, skill := simulationServer(t, 1<<30)
	w := owner.do("GET", "/api/readiness/simulation/sources?worker=worker", "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var listed struct {
		Sources []practice.SourceRow `json:"sources"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &listed)
	if len(listed.Sources) != 2 || listed.Sources[0].OutOfScope != "" || !strings.Contains(listed.Sources[1].OutOfScope, "private step") {
		t.Fatalf("sources %+v", listed.Sources)
	}
	if w = owner.do("POST", "/api/readiness/simulation/draft", `{"worker":"worker","sources":[{"kind":"skill","name":"sign-in"}]}`); w.Code != 422 {
		t.Fatalf("out-of-scope source drafted: %d", w.Code)
	}
	w = owner.do("POST", "/api/readiness/simulation/draft", `{"worker":"worker","sources":[{"kind":"skill","name":"total-file"}],"count":5}`)
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	var started struct {
		Jobs []practice.DraftJob `json:"jobs"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &started)
	var job practice.DraftJob
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		_ = json.Unmarshal(owner.do("GET", "/api/readiness/simulation/draft?job="+started.Jobs[0].ID, "").Body.Bytes(), &job)
		if job.Status == practice.JobDone || job.Status == practice.JobFailed {
			break
		}
	}
	if job.Status != practice.JobDone {
		t.Fatalf("job %+v", job)
	}
	w = owner.do("GET", "/api/readiness/simulation/draft?id="+job.SuiteID, "")
	var view struct {
		GeneratorSHA string   `json:"generator_sha"`
		MockSHA      string   `json:"mock_sha"`
		CasesSHA     string   `json:"cases_sha"`
		Problems     []string `json:"problems"`
		Generator    string   `json:"generator"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &view)
	if len(view.Problems) != 0 || view.Generator != "import sys" || strings.Contains(w.Body.String(), "ordinary3") {
		t.Fatalf("draft view: %s", w.Body.String())
	}
	if w = owner.do("POST", "/api/readiness/simulation/approve", fmt.Sprintf(`{"draft_id":%q,"generator_sha":%q,"mock_sha":"","cases_sha":"other"}`, job.SuiteID, view.GeneratorSHA)); w.Code != 409 {
		t.Fatalf("approved with hashes the owner never saw: %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "changed since it was shown") {
		t.Fatalf("refusal does not say why: %s", w.Body.String())
	}
	if w = owner.do("POST", "/api/readiness/simulation/approve", fmt.Sprintf(`{"draft_id":%q,"generator_sha":%q,"mock_sha":"","cases_sha":%q}`, job.SuiteID, view.GeneratorSHA, view.CasesSHA)); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	run := fmt.Sprintf(`{"suite_id":%q}`, job.SuiteID)
	p.Sandbox = nil
	if w = owner.do("POST", "/api/readiness/simulation/run", run); w.Code != 409 && w.Code != 500 {
		t.Fatalf("run without a sandbox: %d", w.Code)
	}
	p.Sandbox = (*desktop.Manager)(nil)
	if w = owner.do("POST", "/api/readiness/simulation/run", run); w.Code != 409 || !strings.Contains(w.Body.String(), "Stop a worker") {
		t.Fatalf("short memory not refused with a sentence: %d %s", w.Code, w.Body.String())
	}
	p.FreeMemory = func() (uint64, error) { return 8 << 30, nil }
	*skill = `{"name":"total-file","steps":[{"changed":true}]}`
	if w = owner.do("POST", "/api/readiness/simulation/run", run); w.Code != 409 || !strings.Contains(w.Body.String(), "changed") {
		t.Fatalf("changed skill ran: %d %s", w.Code, w.Body.String())
	}
	var states struct {
		Responsibilities []qualification.PracticeResponsibility `json:"responsibilities"`
	}
	_ = json.Unmarshal(owner.do("GET", "/api/readiness/simulation/list?worker=worker", "").Body.Bytes(), &states)
	if len(states.Responsibilities) != 1 || states.Responsibilities[0].Readiness != qualification.NeedsRetesting {
		t.Fatalf("stale suite left %+v", states.Responsibilities)
	}
	r := states.Responsibilities[0]
	if w = owner.do("POST", "/api/readiness/simulation/authorize", fmt.Sprintf(`{"responsibility":%q,"revision":%q,"action":"enable"}`, r.ID, r.Revision)); w.Code != 409 {
		t.Fatalf("enabled without a clean run: %d", w.Code)
	}
	if w = owner.do("GET", "/api/readiness/simulation/file?run=x&case=y&role=output&name=../a", ""); w.Code != 400 {
		t.Fatalf("path in a file name: %d", w.Code)
	}
}

// Enabling a practised skill is refused, and says it is not used yet: nothing starts unattended on a grant (C-14).
func TestEnablingPracticeSaysItIsNotUsedYet(t *testing.T) {
	if practiceGrantsUsed {
		t.Skip("practice grants have a consumer now")
	}
	h := &simulationRoutes{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/readiness/simulation/authorize",
		strings.NewReader(`{"responsibility":"r","revision":"v","action":"enable"}`))
	req.Header.Set("Content-Type", "application/json")
	h.authorize(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "not used yet") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}
