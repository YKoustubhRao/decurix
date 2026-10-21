package httpapi

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/qualification"
	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

func qualificationServer(t *testing.T) *Server {
	t.Helper()
	s := chatServer(t)
	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s.Qualification, err = qualification.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetQualificationOwner(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	return s
}

func apiDefinition() qualification.Definition {
	d := qualification.Definition{Name: "Reconcile", WorkerID: "worker", SourceName: "reconcile", Environment: "local drafts", Scope: "Reconcile supplied CSV", Language: "python", Script: "print('ok')", Image: "sha256:" + strings.Repeat("a", 64), Runtime: qualification.RuntimeVersion, TimeoutSeconds: 5}
	for _, category := range []string{"ordinary", "varied", "missing_input", "invalid_input", "injection"} {
		c := qualification.Case{ID: strings.ReplaceAll(category, "_", "-"), Category: category, Expected: "complete", Checks: []qualification.Check{{File: "out.txt", Kind: "text_equals", Text: "ok"}}}
		if strings.Contains(category, "input") {
			c.Expected = "escalate"
			c.Checks = nil
		}
		d.Cases = append(d.Cases, c)
	}
	return d
}

func TestReadinessOwnerSessionRequiresLocalKeyAndCSRF(t *testing.T) {
	s := qualificationServer(t)
	r := httptest.NewRequest("POST", "/api/readiness/session", strings.NewReader(`{"key":"wrong"}`))
	w := httptest.NewRecorder()
	s.qualificationSessionHandler(w, r)
	if w.Code != 403 {
		t.Fatal("wrong key accepted")
	}
	r = httptest.NewRequest("POST", "/api/readiness/session", strings.NewReader(`{"key":"`+s.qualificationAuth.key+`"}`))
	r.Header.Set("Origin", "https://foreign.example")
	w = httptest.NewRecorder()
	s.qualificationSessionHandler(w, r)
	if w.Code != 403 {
		t.Fatal("foreign origin minted a session")
	}
	r = httptest.NewRequest("POST", "/api/readiness/session", strings.NewReader(`{"key":"`+s.qualificationAuth.key+`"}`))
	w = httptest.NewRecorder()
	s.qualificationSessionHandler(w, r)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var session map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &session)
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("owner cookie insufficiently protected")
	}
	called := false
	handler := s.qualificationOwner(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(204) })
	for _, csrf := range []string{"", session["csrf"]} {
		called = false
		r = httptest.NewRequest("POST", "/api/readiness/run", nil)
		r.AddCookie(cookies[0])
		r.Header.Set("X-Readiness-CSRF", csrf)
		w = httptest.NewRecorder()
		handler(w, r)
		if called != (csrf != "") {
			t.Fatal("CSRF requirement lost")
		}
	}
	r = httptest.NewRequest("GET", "/api/readiness/list", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	w = httptest.NewRecorder()
	handler(w, r)
	if w.Code != 401 {
		t.Fatal("Docker Desktop loopback bypassed owner session")
	}
}

func TestReadinessSourceIsExactAndCannotFallbackToPreview(t *testing.T) {
	s := qualificationServer(t)
	full := strings.Repeat("# tool\n", 4000)
	s.agentRequest = func(ctx context.Context, id, method, path string, body io.Reader) (*http.Response, error) {
		if path != "/playbooks/reconcile/qualification-source" {
			t.Fatalf("preview used: %s", path)
		}
		b, _ := json.Marshal(map[string]string{"script": full, "language": "python"})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(b))}, nil
	}
	script, language, err := s.qualificationSource(context.Background(), "worker", "reconcile")
	if err != nil || script != full || language != "python" {
		t.Fatal("exact source export truncated or rejected")
	}
	s.agentRequest = func(context.Context, string, string, string, io.Reader) (*http.Response, error) {
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("old agent"))}, nil
	}
	if _, _, err = s.qualificationSource(context.Background(), "worker", "reconcile"); err == nil {
		t.Fatal("old agent unexpectedly qualified")
	}
}

func TestAgentCallbacksCannotForgeQualificationEvidence(t *testing.T) {
	s := qualificationServer(t)
	r, err := s.Qualification.Save("", apiDefinition())
	if err != nil {
		t.Fatal(err)
	}
	route := tasks.NewRoute()
	run, err := s.Qualification.BeginRun(r.ID, r.Revision, "practice", route.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	worker := "readiness-" + run.ID
	_, err = s.Tasks.CommitTyped(route, tasks.Worker{ID: worker}, "exam", tasks.TypePractice)
	if err != nil {
		t.Fatal(err)
	}
	for _, handler := range []http.HandlerFunc{s.queryTextHandler, s.agentEventHandler} {
		b, _ := json.Marshal(map[string]any{"desktop_id": worker, "task_id": route.TaskID, "kind": "final", "reply": "Done", "checked": true, "text": "DONE confidence 1"})
		w := postJSON(t, handler, "/api/querytext", string(b))
		if w.Code != 403 {
			t.Fatalf("agent changed evidence: %d %s", w.Code, w.Body.String())
		}
	}
	detail, err := s.Tasks.Get(route.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Task.Status != tasks.StatusRunning || len(detail.Executions[0].Steps) != 0 {
		t.Fatal("forged callback modified task")
	}
	if tasks.CountsForAutonomy(detail.Task.Type) {
		t.Fatal("practice inflated autonomy")
	}
}

func TestWrongTaskVerdictRevokesLinkedResponsibility(t *testing.T) {
	s := qualificationServer(t)
	r, err := s.Qualification.Save("", apiDefinition())
	if err != nil {
		t.Fatal(err)
	}
	route := tasks.NewRoute()
	run, err := s.Qualification.BeginRun(r.ID, r.Revision, "practice", route.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	worker := "readiness-" + run.ID
	_, _ = s.Tasks.CommitTyped(route, tasks.Worker{ID: worker}, "exam", tasks.TypePractice)
	_, _ = s.Tasks.RecordOutcome(worker, route.TaskID, tasks.Outcome{Kind: tasks.OutcomeDone, Reply: "done"})
	req := httptest.NewRequest("POST", "/api/tasks/verdict", strings.NewReader("task_id="+route.TaskID+"&verdict=wrong"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.taskVerdictHandler(w, req)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	r, _ = s.Qualification.Get(r.ID)
	if r.Authorization != qualification.Suspended || r.Readiness != qualification.NeedsRetesting {
		t.Fatal("wrong verdict kept authority")
	}
}

func TestReadinessOwnerKeyExcludedFromBackupAndHostFileReads(t *testing.T) {
	dir := t.TempDir()
	s := qualificationServer(t)
	if err := s.SetQualificationOwner(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(filepath.Join(dir, QualificationOwnerFile)); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if problems := addDataDir(tw, dir); len(problems) != 0 {
		t.Fatal(problems)
	}
	_ = tw.Close()
	if bytes.Contains(b.Bytes(), []byte(s.qualificationAuth.key)) {
		t.Fatal("owner key entered backup")
	}
	w := httptest.NewRecorder()
	s.hostFileAsked(w, httptest.NewRequest("GET", "/api/host-files/read?desktop=worker&path=C:/data/qualification-owner.token", nil))
	if w.Code != 403 {
		t.Fatal("agent can read owner key")
	}
}

func TestReadinessRoutesArePrimaryOnlyAndMutationsNeedOwnerSession(t *testing.T) {
	s := qualificationServer(t)
	gated := http.NewServeMux()
	s.RegisterRoutes(gated)
	for _, path := range []string{"/api/readiness/session", "/api/readiness/run", "/readiness/worker"} {
		_, pattern := gated.Handler(httptest.NewRequest("GET", path, nil))
		if pattern != "/" {
			t.Fatalf("readiness registered on gated listener: %s %s", path, pattern)
		}
	}
	primary := http.NewServeMux()
	s.RegisterLocalRoutes(primary)
	for _, path := range []string{"/api/readiness/define", "/api/readiness/authorize", "/api/readiness/run", "/api/readiness/cancel", "/api/readiness/evidence"} {
		w := httptest.NewRecorder()
		primary.ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader("{}")))
		if w.Code != 401 {
			t.Fatalf("owner mutation exposed: %s %d", path, w.Code)
		}
	}
}

func TestReadinessArtifactsRemainReviewableAfterFailedCheck(t *testing.T) {
	s := qualificationServer(t)
	r, err := s.Qualification.Save("", apiDefinition())
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.Qualification.BeginRun(r.ID, r.Revision, "practice", "task")
	if err != nil {
		t.Fatal(err)
	}
	f := qualification.Finding{CaseID: r.Definition.Cases[0].ID, Passed: false}
	if err = s.Qualification.RecordCase(run.ID, f, map[string][]byte{"out.txt": []byte("wrong")}); err != nil {
		t.Fatal(err)
	}
	_, err = s.Qualification.FinishRun(run.ID, []qualification.Finding{f}, nil)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.qualificationArtifactHandler(w, httptest.NewRequest("GET", "/api/readiness/artifact?id="+run.ID+"&case="+f.CaseID+"&file=out.txt", nil))
	if w.Code != 200 || w.Body.String() != "wrong" || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment;") {
		t.Fatal("failed artifact cannot be reviewed safely")
	}
}
