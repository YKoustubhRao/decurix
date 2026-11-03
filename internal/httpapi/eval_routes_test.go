package httpapi

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/eval"
	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

var evalPaths = []string{"/api/evals/runs", "/api/evals/runs/e1", "/api/evals/runs/e1/rows", "/api/evals/runs/e1/table", "/api/evals/runs/e1/cancel"}

func evalServer(t *testing.T) (*Server, *http.ServeMux, *eval.Store) {
	t.Helper()
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "eval.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store, err := eval.NewStore(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{}
	mux := http.NewServeMux()
	s.RegisterEvalRoutes(mux, &eval.Service{Store: store})
	return s, mux, store
}

// The eval routes are the primary listener's only: the gated mux, which a tunnel reaches, does not have them.
func TestEvalRoutesAreNotOnTheGatedListener(t *testing.T) {
	s := &Server{}
	gated := http.NewServeMux()
	s.RegisterRoutes(gated)
	primary := http.NewServeMux()
	s.RegisterEvalRoutes(primary, nil)
	for _, path := range evalPaths {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if _, pattern := gated.Handler(req); strings.HasPrefix(pattern, "/api/evals") {
			t.Fatalf("%s is on the gated listener", path)
		}
		w := httptest.NewRecorder()
		gated.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s on the gated listener answered %d", path, w.Code)
		}
		w = httptest.NewRecorder()
		primary.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s without a runner: %d", path, w.Code)
		}
	}
}

func TestEvalRoutesRefuseADesktop(t *testing.T) {
	s, mux, _ := evalServer(t)
	s.netsFor = staticNets("172.30.0.0/16", "172.30.0.1")
	for _, path := range evalPaths {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.RemoteAddr = "172.30.0.7:4000"
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s from a desktop: %d", path, w.Code)
		}
	}
}

func multipartBody(t *testing.T, fields map[string]string, files map[string][]byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	for k, v := range files {
		fw, _ := mw.CreateFormFile(k, k+".tar")
		_, _ = fw.Write(v)
	}
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

func TestEvalStartSaysWhatIsWrong(t *testing.T) {
	_, mux, _ := evalServer(t)
	post := func(fields map[string]string, files map[string][]byte) *httptest.ResponseRecorder {
		body, ct := multipartBody(t, fields, files)
		r := httptest.NewRequest(http.MethodPost, "/api/evals/runs", body)
		r.Header.Set("Content-Type", ct)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	if w := post(map[string]string{"arms": "[]", "repeats": "1"}, nil); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "suite tar") {
		t.Fatalf("no suite: %d %s", w.Code, w.Body)
	}
	if w := post(map[string]string{"arms": "nope", "repeats": "1"}, map[string][]byte{"suite": []byte("x")}); w.Code != http.StatusBadRequest {
		t.Fatalf("bad arms: %d", w.Code)
	}
	// A runner not set up refuses with its own sentence, and nothing is stored.
	w := post(map[string]string{"arms": `[{"name":"nothing","kind":"nothing"}]`, "repeats": "1"}, map[string][]byte{"suite": []byte("x")})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "not set up") {
		t.Fatalf("not set up: %d %s", w.Code, w.Body)
	}
}

func TestEvalRunRowsAndCancel(t *testing.T) {
	_, mux, store := evalServer(t)
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}
	if w := get("/api/evals/runs/e9"); w.Code != http.StatusNotFound {
		t.Fatalf("unknown run: %d", w.Code)
	}
	if err := store.Create(eval.Run{ID: "e1", SuiteName: "sample", SuiteSHA: "abc", Arms: []eval.Arm{{Name: "nothing", Kind: eval.ArmNothing}},
		Cases: []string{"c1"}, Repeats: 1, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	_ = store.AddRow(eval.Row{RunID: "e1", Arm: "nothing", Repeat: 1, CaseID: "c1", Observed: eval.OutcomeComplete, FalseDone: true, At: time.Now()})
	var list struct {
		Runs []eval.Run `json:"runs"`
	}
	if w := get("/api/evals/runs"); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list.Runs) != 1 {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	var one struct {
		Run    eval.Run `json:"run"`
		Active bool     `json:"active"`
	}
	if w := get("/api/evals/runs/e1"); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &one) != nil || one.Run.Status != eval.RunRunning || one.Active {
		t.Fatalf("run: %d %s", w.Code, w.Body)
	}
	var rows struct {
		Rows []eval.Row `json:"rows"`
	}
	if w := get("/api/evals/runs/e1/rows"); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &rows) != nil || len(rows.Rows) != 1 || !rows.Rows[0].FalseDone {
		t.Fatalf("rows: %d %s", w.Code, w.Body)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/evals/runs/e1/cancel", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"cancelled"`) {
		t.Fatalf("cancel: %d %s", w.Code, w.Body)
	}
	if w := get("/api/evals/runs/e1/nope"); w.Code != http.StatusNotFound {
		t.Fatalf("unknown sub-route: %d", w.Code)
	}
}
