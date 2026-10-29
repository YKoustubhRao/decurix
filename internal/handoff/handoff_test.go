package handoff

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// --- fakes -----------------------------------------------------------------

type busyErr struct{ why string }

func (e *busyErr) Error() string       { return "busy: " + e.why }
func (e *busyErr) DesktopBusy() string { return e.why }

type sent struct {
	worker, content, dir string
	files                []File
}

type fakeSubmit struct {
	mu    sync.Mutex
	calls []sent
	busy  map[string]bool
	fail  error
	n     int
}

func (f *fakeSubmit) SubmitHandoff(_ context.Context, worker, content, dir string, files []File) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.busy[worker] {
		return "", &busyErr{why: "a task is running"}
	}
	if f.fail != nil {
		return "", f.fail
	}
	f.n++
	f.calls = append(f.calls, sent{worker, content, dir, files})
	return "task-" + worker + "-" + string(rune('0'+f.n)), nil
}

type fakeTasks struct {
	mu       sync.Mutex
	outcomes map[string]TaskOutcome
	busy     map[string]string
}

func (f *fakeTasks) TaskOutcome(id string) (TaskOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.outcomes[id], nil
}

func (f *fakeTasks) Busy(worker string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.busy[worker], nil
}

func (f *fakeTasks) set(id string, o TaskOutcome) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o.Found = true
	f.outcomes[id] = o
}

type fakeWorkers map[string]bool

func (f fakeWorkers) Resolve(_ context.Context, raw string) (string, bool) {
	id := strings.ToLower(strings.TrimSpace(raw))
	return id, f[id]
}

type fakeFinder map[string][]string

func (f fakeFinder) Holders(_ context.Context, role, skill string) ([]string, error) {
	if role != "" {
		return f["role:"+role], nil
	}
	return f["skill:"+skill], nil
}

type fakeFS struct {
	mu      sync.Mutex
	files   map[string]map[string][]byte // worker -> path -> data
	written map[string][]File            // worker+dir -> files
}

func (f *fakeFS) ReadFiles(_ context.Context, worker string, paths []string) ([]File, error) {
	var out []File
	for _, p := range paths {
		data, ok := f.files[worker][p]
		if !ok {
			return nil, errors.New("no such file")
		}
		out = append(out, File{Name: p, Data: data})
	}
	return out, nil
}

func (f *fakeFS) WriteFiles(_ context.Context, worker, dir string, files []File) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.written[worker+dir] = files
	return nil
}

type rig struct {
	svc    *Service
	submit *fakeSubmit
	tasks  *fakeTasks
	fs     *fakeFS
	now    time.Time
}

func newRig(t *testing.T) *rig {
	t.Helper()
	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{
		submit: &fakeSubmit{busy: map[string]bool{}},
		tasks:  &fakeTasks{outcomes: map[string]TaskOutcome{}, busy: map[string]string{}},
		fs: &fakeFS{files: map[string]map[string][]byte{
			"a": {"/home/headless/in/data.csv": []byte("x,y\n1,2\n")},
			"b": {"/home/headless/out/summary.txt": []byte("total 3")},
		}, written: map[string][]File{}},
		now: time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC),
	}
	clock := func() time.Time { return r.now }
	store.now = clock
	r.svc = &Service{
		Store: store, Submit: r.submit, Tasks: r.tasks,
		Workers: fakeWorkers{"a": true, "b": true, "c": true, "d": true},
		Finder: fakeFinder{
			"role:reviewer":  {"a", "b", "c"},
			"skill:invoices": {"c", "d"},
			"skill:nobody":   {"a"},
		},
		Reader: r.fs, Writer: r.fs, now: clock,
	}
	return r
}

func yes() *bool { b := true; return &b }
func no() *bool  { b := false; return &b }

// --- service ---------------------------------------------------------------

func TestMigrateTwiceIsHarmless(t *testing.T) {
	db, _ := sqlitedb.OpenMemory()
	defer db.Close()
	if _, err := NewStore(db); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(db); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

func TestDelegateToNamedWorkerVerifiedIsDone(t *testing.T) {
	r := newRig(t)
	h, err := r.svc.Delegate(context.Background(), DelegateRequest{
		FromWorker: "A", FromTask: "t-a", ToWorker: "b", Request: "Summarise the sheet.",
		Inputs: map[string]string{"period": "last month"}, Files: []string{"~/in/data.csv"},
		Expected: "a summary file with the total",
	})
	if err != nil {
		t.Fatal(err)
	}
	if h.Status != StatusRunning || h.ToWorker != "b" || h.TaskID == "" {
		t.Fatalf("not sent: %+v", h)
	}
	call := r.submit.calls[0]
	for _, want := range []string{"Summarise the sheet.", "- period: last month", "data.csv", "a summary file with the total", h.ID} {
		if !strings.Contains(call.content, want) {
			t.Errorf("request lacks %q:\n%s", want, call.content)
		}
	}
	if call.dir != InboxDir(h.ID) || len(call.files) != 1 || string(call.files[0].Data) != "x,y\n1,2\n" {
		t.Errorf("files not carried: %+v", call)
	}

	r.tasks.set(h.TaskID, TaskOutcome{Status: tasks.StatusDone, Checked: yes(),
		Report: "Wrote /home/headless/out/summary.txt.\n\nChecked against the files I wrote: total 3 is in the file."})
	got, err := r.svc.Get(context.Background(), h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDone || !got.Result.Verified {
		t.Fatalf("want done+verified, got %+v", got)
	}
	if len(got.Result.Evidence) != 1 || !strings.HasPrefix(got.Result.Evidence[0], "Checked against the files I wrote:") {
		t.Errorf("evidence: %v", got.Result.Evidence)
	}
	if len(got.Result.Files) != 1 || got.Result.Files[0] != ReturnDir(h.ID)+"/summary.txt" {
		t.Errorf("files: %v", got.Result.Files)
	}
	if w := r.fs.written["a"+ReturnDir(h.ID)]; len(w) != 1 || string(w[0].Data) != "total 3" {
		t.Errorf("result file not copied back: %+v", w)
	}
}

func TestUncheckedResultIsNeverDone(t *testing.T) {
	for name, checked := range map[string]*bool{"not checked": no(), "not said": nil} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			h, err := r.svc.Delegate(context.Background(), DelegateRequest{FromWorker: "a", ToWorker: "b", Request: "do it"})
			if err != nil {
				t.Fatal(err)
			}
			r.tasks.set(h.TaskID, TaskOutcome{Status: tasks.StatusDone, Checked: checked, Report: "All done!"})
			got, _ := r.svc.Get(context.Background(), h.ID)
			if got.Status != StatusNotDone || got.Result.Verified {
				t.Fatalf("an unverified result must be not_done: %+v", got)
			}
			if got.Result.Report != "All done!" || !strings.Contains(got.Reason, "did not confirm") {
				t.Errorf("report/reason: %+v", got)
			}
		})
	}
}

func TestErrorQueryAndAbandoned(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	h, _ := r.svc.Delegate(ctx, DelegateRequest{FromWorker: "a", ToWorker: "b", Request: "do it"})

	r.tasks.set(h.TaskID, TaskOutcome{Status: tasks.StatusQuery, OpenQuestions: []string{"Which month?"}})
	got, _ := r.svc.Get(ctx, h.ID)
	if got.Status != StatusRunning || len(got.Result.OpenQuestions) != 1 || !strings.Contains(got.Reason, "asked a person") {
		t.Fatalf("query: %+v", got)
	}

	r.tasks.set(h.TaskID, TaskOutcome{Status: tasks.StatusError, Checked: yes(), Report: "Could not open it."})
	got, _ = r.svc.Get(ctx, h.ID)
	if got.Status != StatusFailed || got.Result.Verified || got.Result.Report != "Could not open it." {
		t.Fatalf("error: %+v", got)
	}

	// Final stays final, whatever the task says next.
	r.tasks.set(h.TaskID, TaskOutcome{Status: tasks.StatusDone, Checked: yes()})
	if got, _ = r.svc.Get(ctx, h.ID); got.Status != StatusFailed {
		t.Fatalf("a final handoff changed: %+v", got)
	}

	h2, _ := r.svc.Delegate(ctx, DelegateRequest{FromWorker: "a", ToWorker: "c", Request: "x"})
	r.tasks.set(h2.TaskID, TaskOutcome{Status: tasks.StatusAbandoned})
	if got, _ = r.svc.Get(ctx, h2.ID); got.Status != StatusFailed {
		t.Fatalf("abandoned: %+v", got)
	}
}

func TestBusyWorkerQueuesThenTickSends(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.submit.busy["b"] = true
	h, err := r.svc.Delegate(ctx, DelegateRequest{FromWorker: "a", ToWorker: "b", Request: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if h.Status != StatusQueued || !strings.Contains(h.Reason, "waiting for worker b") {
		t.Fatalf("want queued: %+v", h)
	}
	r.submit.busy["b"] = false
	r.svc.Tick(ctx)
	got, _ := r.svc.Store.Get(h.ID)
	if got.Status != StatusRunning || got.TaskID == "" {
		t.Fatalf("tick did not send: %+v", got)
	}
}

func TestQueueTimeoutFails(t *testing.T) {
	r := newRig(t)
	r.submit.busy["b"] = true
	h, _ := r.svc.Delegate(context.Background(), DelegateRequest{FromWorker: "a", ToWorker: "b", Request: "x"})
	r.now = r.now.Add(31 * time.Minute)
	r.svc.Tick(context.Background())
	got, _ := r.svc.Store.Get(h.ID)
	if got.Status != StatusFailed || !strings.Contains(got.Reason, "no worker took it") {
		t.Fatalf("want failed on timeout: %+v", got)
	}
}

func TestRoleResolutionPicksAFreeHolderOtherThanTheSender(t *testing.T) {
	r := newRig(t)
	r.tasks.busy["b"] = "running a task"
	h, err := r.svc.Delegate(context.Background(), DelegateRequest{FromWorker: "a", ToRole: "reviewer", Request: "review"})
	if err != nil {
		t.Fatal(err)
	}
	if h.ToWorker != "c" || h.Status != StatusRunning {
		t.Fatalf("want c (a is the sender, b busy): %+v", h)
	}
}

func TestRoleAllBusyQueuesNoHolderFails(t *testing.T) {
	r := newRig(t)
	r.tasks.busy["c"], r.tasks.busy["d"] = "x", "y"
	h, _ := r.svc.Delegate(context.Background(), DelegateRequest{FromWorker: "a", ToSkill: "invoices", Request: "x"})
	if h.Status != StatusQueued || h.ToWorker != "" || !strings.Contains(h.Reason, "busy") {
		t.Fatalf("want queued: %+v", h)
	}
	h2, _ := r.svc.Delegate(context.Background(), DelegateRequest{FromWorker: "a", ToSkill: "nobody", Request: "x"})
	if h2.Status != StatusFailed || !strings.Contains(h2.Reason, "no other worker holds") {
		t.Fatalf("want failed: %+v", h2)
	}
}

func TestValidation(t *testing.T) {
	r := newRig(t)
	cases := map[string]DelegateRequest{
		"unknown sender":   {FromWorker: "zz", ToWorker: "b", Request: "x"},
		"unknown receiver": {FromWorker: "a", ToWorker: "zz", Request: "x"},
		"no target":        {FromWorker: "a", Request: "x"},
		"two targets":      {FromWorker: "a", ToWorker: "b", ToRole: "reviewer", Request: "x"},
		"no request":       {FromWorker: "a", ToWorker: "b"},
		"file outside":     {FromWorker: "a", ToWorker: "b", Request: "x", Files: []string{"/etc/passwd"}},
		"file escapes":     {FromWorker: "a", ToWorker: "b", Request: "x", Files: []string{"/home/headless/../root/x"}},
		"self":             {FromWorker: "a", ToWorker: "a", Request: "x"},
		"long request":     {FromWorker: "a", ToWorker: "b", Request: strings.Repeat("x", MaxRequestBytes+1)},
	}
	for name, req := range cases {
		_, err := r.svc.Delegate(context.Background(), req)
		var in *InputError
		if !errors.As(err, &in) {
			t.Errorf("%s: want InputError, got %v", name, err)
		}
	}
}

func TestHandingBackUpTheChainIsRefusedAndDepthBounded(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	first, _ := r.svc.Delegate(ctx, DelegateRequest{FromWorker: "a", FromTask: "t-a", ToWorker: "b", Request: "x"})
	_, err := r.svc.Delegate(ctx, DelegateRequest{FromWorker: "b", FromTask: first.TaskID, ToWorker: "a", Request: "y"})
	if err == nil || !strings.Contains(err.Error(), "waiting on this work") {
		t.Fatalf("hand-back not refused: %v", err)
	}
	second, err := r.svc.Delegate(ctx, DelegateRequest{FromWorker: "b", FromTask: first.TaskID, ToWorker: "c", Request: "y"})
	if err != nil || second.Depth != 1 {
		t.Fatalf("second hop: %+v %v", second, err)
	}
	third, err := r.svc.Delegate(ctx, DelegateRequest{FromWorker: "c", FromTask: second.TaskID, ToWorker: "d", Request: "z"})
	if err != nil || third.Depth != 2 {
		t.Fatalf("third hop: %+v %v", third, err)
	}
	// A role pick skips every worker up the chain.
	r.svc.Finder = fakeFinder{"role:reviewer": {"a", "b", "c"}}
	h, _ := r.svc.Delegate(ctx, DelegateRequest{FromWorker: "d", FromTask: third.TaskID, ToRole: "reviewer", Request: "w"})
	if h.Status != StatusFailed {
		t.Fatalf("role pick reached a waiting worker: %+v", h)
	}
}

func TestCancel(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	h, _ := r.svc.Delegate(ctx, DelegateRequest{FromWorker: "a", ToWorker: "b", Request: "x"})
	if _, err := r.svc.Cancel(ctx, h.ID, "c"); err == nil {
		t.Fatal("another worker cancelled it")
	}
	got, err := r.svc.Cancel(ctx, h.ID, "a")
	if err != nil || got.Status != StatusCancelled || !strings.Contains(got.Reason, "not stopped") {
		t.Fatalf("cancel: %+v %v", got, err)
	}
	r.tasks.set(h.TaskID, TaskOutcome{Status: tasks.StatusDone, Checked: yes()})
	if got, _ = r.svc.Get(ctx, h.ID); got.Status != StatusCancelled {
		t.Fatalf("a cancelled handoff was collected: %+v", got)
	}
	if _, err := r.svc.Cancel(ctx, "ho_missing", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestAsk(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()

	r.tasks.busy["c"], r.tasks.busy["d"] = "x", "y"
	h, err := r.svc.Ask(ctx, AskRequest{FromWorker: "a", Skill: "invoices", Question: "Which column holds the due date?"})
	if err != nil || h.Status != StatusFailed || !strings.Contains(h.Reason, "ask a person") {
		t.Fatalf("no free holder must fail at once: %+v %v", h, err)
	}

	r.tasks.busy["d"] = ""
	h, err = r.svc.Ask(ctx, AskRequest{FromWorker: "a", Skill: "invoices", Question: "Which column holds the due date?"})
	if err != nil || h.Status != StatusRunning || h.ToWorker != "d" {
		t.Fatalf("ask: %+v %v", h, err)
	}
	content := r.submit.calls[len(r.submit.calls)-1].content
	if !strings.Contains(content, "I do not know") || !strings.Contains(content, "Which column holds the due date?") {
		t.Errorf("ask request: %s", content)
	}

	r.tasks.set(h.TaskID, TaskOutcome{Status: tasks.StatusDone, Report: "Column F."})
	got, _ := r.svc.Get(ctx, h.ID)
	if got.Status != StatusNotDone || got.Result.Verified || got.Result.Report != "Column F." {
		t.Fatalf("an unchecked answer is not verified: %+v", got)
	}

	h, _ = r.svc.Ask(ctx, AskRequest{FromWorker: "a", Skill: "invoices", Question: "q"})
	r.now = r.now.Add(16 * time.Minute)
	if got, _ = r.svc.Get(ctx, h.ID); got.Status != StatusFailed {
		t.Fatalf("ask timeout: %+v", got)
	}
}

// --- HTTP ------------------------------------------------------------------

func serve(t *testing.T, r *rig) *httptest.Server {
	mux := http.NewServeMux()
	(&Handler{Service: r.svc, AskWaitCeiling: 3 * time.Second}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, url string, body any) (*http.Response, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func getJSON(t *testing.T, url string) (*http.Response, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func TestHTTPRoutes(t *testing.T) {
	r := newRig(t)
	srv := serve(t, r)

	resp, out := post(t, srv.URL+"/api/handoffs", map[string]any{
		"desktop_id": "a", "from_task": "t-a", "to_worker": "b", "request": "Do it.",
		"inputs": map[string]string{"k": "v"}, "expected": "a file",
	})
	if resp.StatusCode != http.StatusCreated || out["status"] != StatusRunning {
		t.Fatalf("create: %d %v", resp.StatusCode, out)
	}
	id := out["id"].(string)
	taskID := out["task_id"].(string)

	r.tasks.set(taskID, TaskOutcome{Status: tasks.StatusDone, Checked: yes(), Report: "Done. Checked on the screen: saved."})
	resp, out = getJSON(t, srv.URL+"/api/handoffs/get?id="+id)
	result := out["result"].(map[string]any)
	if resp.StatusCode != 200 || out["status"] != StatusDone || result["verified"] != true {
		t.Fatalf("get: %d %v", resp.StatusCode, out)
	}

	resp, out = getJSON(t, srv.URL+"/api/handoffs?worker=b")
	if list := out["handoffs"].([]any); resp.StatusCode != 200 || len(list) != 1 {
		t.Fatalf("list: %v", out)
	}
	if _, out = getJSON(t, srv.URL+"/api/handoffs?worker=zz"); len(out["handoffs"].([]any)) != 0 {
		t.Fatalf("list for another worker: %v", out)
	}

	if resp, _ = getJSON(t, srv.URL+"/api/handoffs/get?id=ho_nope"); resp.StatusCode != 404 {
		t.Fatalf("missing: %d", resp.StatusCode)
	}
	resp, out = post(t, srv.URL+"/api/handoffs", map[string]any{"desktop_id": "a", "request": "x"})
	if resp.StatusCode != 400 || !strings.Contains(out["error"].(string), "exactly one") {
		t.Fatalf("bad create: %d %v", resp.StatusCode, out)
	}
	if resp, _ = post(t, srv.URL+"/api/handoffs/get", map[string]any{}); resp.StatusCode != 405 {
		t.Fatalf("method: %d", resp.StatusCode)
	}

	resp2, err := http.Post(srv.URL+"/api/handoffs", "application/json", strings.NewReader("{not json"))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 400 {
		t.Fatalf("bad json: %d", resp2.StatusCode)
	}

	_, out = post(t, srv.URL+"/api/handoffs", map[string]any{"desktop_id": "a", "to_worker": "c", "request": "x"})
	resp, out = post(t, srv.URL+"/api/handoffs/cancel", map[string]any{"id": out["id"], "desktop_id": "a"})
	if resp.StatusCode != 200 || out["status"] != StatusCancelled {
		t.Fatalf("cancel: %d %v", resp.StatusCode, out)
	}
}

func TestHTTPAskWaitsForTheAnswer(t *testing.T) {
	r := newRig(t)
	srv := serve(t, r)

	// The holder answers while the request is held open.
	go func() {
		for i := 0; i < 50; i++ {
			time.Sleep(50 * time.Millisecond)
			r.submit.mu.Lock()
			n := len(r.submit.calls)
			r.submit.mu.Unlock()
			if n > 0 {
				r.tasks.set("task-c-1", TaskOutcome{Status: tasks.StatusDone, Checked: yes(), Report: "Column F."})
				return
			}
		}
	}()

	resp, out := post(t, srv.URL+"/api/handoffs/ask", map[string]any{
		"from_worker": "a", "skill": "invoices", "question": "Which column?", "wait": 10,
	})
	if resp.StatusCode != 200 || out["answer"] != "Column F." || out["verified"] != true || out["answered_by"] != "c" {
		t.Fatalf("ask: %d %v", resp.StatusCode, out)
	}

	resp, out = post(t, srv.URL+"/api/handoffs/ask", map[string]any{"from_worker": "a", "skill": "nobody", "question": "q"})
	if resp.StatusCode != 200 || out["answered_by"] != "" || out["handoff"].(map[string]any)["status"] != StatusFailed {
		t.Fatalf("ask with no holder: %v", out)
	}
}

func TestInternalErrorsAreNotEchoed(t *testing.T) {
	r := newRig(t)
	r.submit.fail = errors.New("secret internal detail")
	srv := serve(t, r)
	_, out := post(t, srv.URL+"/api/handoffs", map[string]any{"desktop_id": "a", "to_worker": "b", "request": "x"})
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), "secret internal detail") || out["status"] != StatusFailed {
		t.Fatalf("internal error leaked or not failed: %s", b)
	}
}

// --- helpers ---------------------------------------------------------------

func TestReadTarFiles(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "out/", Mode: 0o755})
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: "out/link", Linkname: "/etc/passwd"})
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "out/a.txt", Size: 3, Mode: 0o644})
	_, _ = tw.Write([]byte("abc"))
	_ = tw.Close()

	files, err := ReadTarFiles(bytes.NewReader(buf.Bytes()), 100)
	if err != nil || len(files) != 1 || files[0].Name != "a.txt" || string(files[0].Data) != "abc" {
		t.Fatalf("got %+v %v", files, err)
	}
	if _, err := ReadTarFiles(bytes.NewReader(buf.Bytes()), 2); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("budget: %v", err)
	}
}

func TestEvidenceAndFilesIn(t *testing.T) {
	report := "Made ~/out/report.xlsx and /home/headless/out/notes.txt.\n" +
		"Also looked at /home/headless/out (a folder) and /etc/hosts.\n\n" +
		"Checked on the screen: the record shows total 1,200.\n\n" +
		"Values read from the documents:\n- total: 1,200 (invoice.pdf)\n- date: 3 Oct\n\nThanks."
	ev := EvidenceIn(report)
	if len(ev) != 3 || !strings.HasPrefix(ev[0], "Checked on the screen:") || ev[2] != "- date: 3 Oct" {
		t.Errorf("evidence: %q", ev)
	}
	files := FilesIn(report)
	if len(files) != 2 || files[0] != "/home/headless/out/report.xlsx" || files[1] != "/home/headless/out/notes.txt" {
		t.Errorf("files: %q", files)
	}
}
