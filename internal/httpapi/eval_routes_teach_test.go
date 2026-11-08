package httpapi

// Tests of the teach routes (eval_routes.go, RegisterEvalTeachRoutes) and of from_worker on the run route: a normal
// desktop gets a case's inputs, its mock and its host names; nothing else is mapped; the routes are the primary
// listener's only.

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/eval"
	"github.com/YKoustubhRao/decurix/internal/practice"
)

type fakeTeach struct {
	copied  map[string][]desktop.File
	started []string
	dir     string
	port    int
	hosts   []string
	target  string
	stopped string
	unmap   bool
	err     error
}

func (f *fakeTeach) CopyFiles(_ context.Context, id, dir string, files []desktop.File) error {
	if f.err != nil {
		return f.err
	}
	if f.copied == nil {
		f.copied = map[string][]desktop.File{}
	}
	f.copied[id+":"+dir] = append(f.copied[id+":"+dir], files...)
	return nil
}
func (f *fakeTeach) StartWorkerProcess(_ context.Context, id, dir string, argv []string, port, _ int) error {
	f.started, f.dir, f.port = argv, dir, port
	return nil
}
func (f *fakeTeach) StopWorkerProcess(_ context.Context, id, dir string) error {
	f.stopped = id + ":" + dir
	return f.err
}
func (f *fakeTeach) MapBrowserHosts(_ context.Context, id string, hosts []string, target string) error {
	f.hosts, f.target = hosts, target
	return nil
}
func (f *fakeTeach) UnmapBrowserHosts(_ context.Context, id string) error {
	f.unmap = true
	return nil
}

// teachSuite is a one-case suite on a mock answering at hosts, with one input file.
func teachSuite(t *testing.T, hosts ...string) []byte {
	t.Helper()
	js := func(v any) []byte {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	files := map[string][]byte{
		"suite.json": js(map[string]any{"schema": eval.SuiteSchema, "name": "teach-test", "description": "",
			"mocks": map[string]any{"app": map[string]any{"hosts": hosts}}, "budget": map[string]any{"max_tokens": 5000, "max_minutes": 5},
			"cases": []string{"enter-one"}}),
		"mocks/app/mock_app.py":        []byte("print('mock')\n"),
		"cases/enter-one/fixture.json": []byte(`{"collections": {"bills": []}}`),
		"cases/enter-one/files/in.txt": []byte("bill 1\n"),
		"cases/enter-one/case.json": js(map[string]any{"schema": eval.CaseSchema, "id": "enter-one", "category": "ordinary", "kind": "web_form",
			"request": "Enter the bill in in.txt.", "start": map[string]any{"files": []string{"in.txt"}, "mock": "app", "fixture": "fixture.json"},
			"facts": []any{}, "expected": map[string]any{"outcome": "complete", "graders": []any{map[string]any{"kind": "mock_records",
				"collection": "bills", "exact_count": true, "records": []any{map[string]any{"number": "B-1"}}}}},
			"oracle": []any{"curl -d number=B-1 x"}}),
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for n, b := range files {
		_ = tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(b))})
		_, _ = tw.Write(b)
	}
	_ = tw.Close()
	return buf.Bytes()
}

func teachServer(t *testing.T, d TeachDesktops) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	(&Server{}).RegisterEvalTeachRoutes(mux, d)
	return mux
}

func postTeach(t *testing.T, mux *http.ServeMux, fields map[string]string, suite []byte) *httptest.ResponseRecorder {
	t.Helper()
	files := map[string][]byte{}
	if suite != nil {
		files["suite"] = suite
	}
	body, ct := multipartBody(t, fields, files)
	r := httptest.NewRequest(http.MethodPost, evalTeachPath, body)
	r.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func TestTeachPutsTheCaseOnADesktop(t *testing.T) {
	d := &fakeTeach{}
	mux := teachServer(t, d)
	w := postTeach(t, mux, map[string]string{"desktop": "desk-7", "case": "enter-one"}, teachSuite(t, "app.example.test", "files.example.test"))
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	in := d.copied["desk-7:/home/headless/work/practice/enter-one"]
	if len(in) != 1 || in[0].Name != "in.txt" || string(in[0].Data) != "bill 1\n" {
		t.Fatalf("inputs: %+v", d.copied)
	}
	mock := d.copied["desk-7:"+teachMockDir]
	if len(mock) != 2 || mock[0].Name != "mock_app.py" || mock[1].Name != "fixture.json" || !strings.Contains(string(mock[1].Data), "bills") {
		t.Fatalf("mock files: %+v", mock)
	}
	if d.dir != teachMockDir || d.port != teachMockPort || !strings.Contains(strings.Join(d.started, " "),
		"mock_app.py --host 127.0.0.1 --port 18080 --fixture fixture.json --state state.json") {
		t.Fatalf("started %v in %s on %d", d.started, d.dir, d.port)
	}
	if strings.Join(d.hosts, ",") != "app.example.test,files.example.test" || d.target != "127.0.0.1:18080" {
		t.Fatalf("mapped %v to %s", d.hosts, d.target)
	}
	if !strings.Contains(w.Body.String(), "open it again") {
		t.Fatalf("answer: %s", w.Body)
	}
}

func TestTeachRefusesWhatItMustNotDo(t *testing.T) {
	d := &fakeTeach{}
	mux := teachServer(t, d)
	good := teachSuite(t, "app.example.test")
	for name, c := range map[string]struct {
		fields map[string]string
		suite  []byte
		code   int
	}{
		"a practice desktop":  {map[string]string{"desktop": "practice-e1-a1", "case": "enter-one"}, good, http.StatusBadRequest},
		"a readiness desktop": {map[string]string{"desktop": "readiness-x", "case": "enter-one"}, good, http.StatusBadRequest},
		"no desktop":          {map[string]string{"case": "enter-one"}, good, http.StatusBadRequest},
		"a missing case":      {map[string]string{"desktop": "desk-7", "case": "nope"}, good, http.StatusBadRequest},
		"no suite":            {map[string]string{"desktop": "desk-7", "case": "enter-one"}, nil, http.StatusBadRequest},
		"a real site's name":  {map[string]string{"desktop": "desk-7", "case": "enter-one"}, teachSuite(t, "bank.example.com"), http.StatusBadRequest},
	} {
		d.copied, d.started, d.hosts = nil, nil, nil
		w := postTeach(t, mux, c.fields, c.suite)
		if w.Code != c.code {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
		if d.copied != nil || d.started != nil || d.hosts != nil {
			t.Errorf("%s: the desktop was changed", name)
		}
	}
	// A desktop that is not running is said so; an internal error is never echoed.
	d.err = desktop.ErrDesktopNotRunning
	if w := postTeach(t, mux, map[string]string{"desktop": "desk-7", "case": "enter-one"}, good); w.Code != http.StatusConflict {
		t.Fatalf("not running: %d %s", w.Code, w.Body)
	}
	d.err = errors.New("exec failed: secret detail")
	if w := postTeach(t, mux, map[string]string{"desktop": "desk-7", "case": "enter-one"}, good); w.Code != http.StatusInternalServerError ||
		strings.Contains(w.Body.String(), "secret detail") {
		t.Fatalf("internal: %d %s", w.Code, w.Body)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, evalTeachPath, nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", w.Code)
	}
}

func TestTeachStopUndoesIt(t *testing.T) {
	d := &fakeTeach{}
	mux := teachServer(t, d)
	r := httptest.NewRequest(http.MethodPost, evalTeachStopPath, strings.NewReader(url.Values{"desktop": {"desk-7"}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK || d.stopped != "desk-7:"+teachMockDir || !d.unmap {
		t.Fatalf("%d %s stopped %q unmap %v", w.Code, w.Body, d.stopped, d.unmap)
	}
}

// The teach routes start a process in a desktop and change its browser: the primary listener's only.
func TestTeachRoutesAreNotOnTheGatedListener(t *testing.T) {
	s := &Server{}
	gated := http.NewServeMux()
	s.RegisterRoutes(gated)
	for _, path := range []string{evalTeachPath, evalTeachStopPath} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		if _, pattern := gated.Handler(req); strings.HasPrefix(pattern, "/api/evals") {
			t.Fatalf("%s is on the gated listener", path)
		}
		w := httptest.NewRecorder()
		gated.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s on the gated listener answered %d", path, w.Code)
		}
	}
	// And a desktop cannot call them.
	s.netsFor = staticNets("172.30.0.0/16", "172.30.0.1")
	mux := http.NewServeMux()
	s.RegisterEvalTeachRoutes(mux, &fakeTeach{})
	r := httptest.NewRequest(http.MethodPost, evalTeachPath, nil)
	r.RemoteAddr = "172.30.0.7:4000"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("from a desktop: %d", w.Code)
	}
}

// failingWorkers is a worker that cannot be read; it keeps the id it was asked for.
type failingWorkers struct{ asked string }

func (f *failingWorkers) PracticeAgentTree(_ context.Context, id string) ([]byte, string, error) {
	f.asked = id
	return nil, "", errors.New("no such container")
}
func (f *failingWorkers) PracticeSnapshot(context.Context, string) ([]byte, string, error) {
	return nil, "", errors.New("no")
}

// nilSandbox stands in for the practice sandbox: Start refuses before it is used.
type nilSandbox struct{ eval.Sandbox }

// from_worker reaches the runner as the run's FromWorker, and a practice desktop is refused at the route.
func TestEvalStartPassesFromWorker(t *testing.T) {
	_, _, store := evalServer(t)
	workers := &failingWorkers{}
	mux := http.NewServeMux()
	(&Server{}).RegisterEvalRoutes(mux, &eval.Service{Store: store, Sandbox: nilSandbox{}, Broker: &practice.Broker{},
		BrokerURL: func(context.Context) (string, error) { return "", nil }, Practice: &practice.Service{}, Workers: workers})
	post := func(from string) *httptest.ResponseRecorder {
		body, ct := multipartBody(t, map[string]string{"arms": `[{"name":"nothing","kind":"nothing"}]`, "repeats": "1", "from_worker": from},
			map[string][]byte{"suite": teachSuite(t, "app.example.test")})
		r := httptest.NewRequest(http.MethodPost, "/api/evals/runs", body)
		r.Header.Set("Content-Type", ct)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	if w := post(" desk-7 "); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "could not be read") ||
		strings.Contains(w.Body.String(), "no such container") || workers.asked != "desk-7" {
		t.Fatalf("from_worker: %d %s (asked %q)", w.Code, w.Body, workers.asked)
	}
	workers.asked = ""
	if w := post("practice-e1-a1"); w.Code != http.StatusBadRequest || workers.asked != "" {
		t.Fatalf("a practice desktop: %d %s", w.Code, w.Body)
	}
}
