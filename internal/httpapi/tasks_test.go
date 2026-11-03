package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/conversation"
	"github.com/YKoustubhRao/decurix/internal/tasks"
	"github.com/YKoustubhRao/decurix/internal/webui"
)

func postForm(t *testing.T, handler http.HandlerFunc, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	return postAs(t, handler, path, "application/x-www-form-urlencoded", body)
}

// postAs POSTs body, of contentType, to handler.
func postAs(t *testing.T, handler http.HandlerFunc, path, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)

	rec := httptest.NewRecorder()
	handler(rec, req)

	return rec
}

// deliver is sendRequestHandler minus the Docker call: route, then record as
// the desktop having accepted it.
func deliver(t *testing.T, s *Server, desktopID, content string) tasks.Route {
	t.Helper()

	if err := s.Tasks.UpsertWorker(tasks.Worker{ID: desktopID, Engine: "claude", Model: "claude-opus-5"}); err != nil {
		t.Fatal(err)
	}

	route, _ := s.recordDelivered(context.Background(), desktopID, content, s.routeRequest(desktopID, content))
	return route
}

// One task end to end as the control plane sees it - a chat request, the
// agent's steps (one of them on screen, with screenshots), an approval, the
// human's reply, the answer, and a verdict - lands in the task records, the
// chat shows the verdict, and the report and both pages render it.
func TestTaskRecordEndToEnd(t *testing.T) {
	s := chatServer(t)
	s.Web = webui.NewRenderer()

	route := deliver(t, s, "claude-box", "pay invoice INV-1042 in the admin panel")

	shot := base64.StdEncoding.EncodeToString(tinyPNG(t))

	postJSON(t, s.agentEventHandler, "/api/agent/events",
		`{"desktop_id":"claude-box","task_id":"`+route.TaskID+`","text":"click button \"Pay now\" - stopped",
		  "action":{"kind":"click","outcome":"needs_approval","target":"button \"Pay now\""},"before_png":"`+shot+`"}`)

	postJSON(t, s.queryTextHandler, "/api/querytext",
		`{"desktop_id":"claude-box","task_id":"`+route.TaskID+`","content":"DECURIX QUERY REQUEST","kind":"approval",
		  "reply":"Approval needed before I click button \"Pay now\": looks like a payment.","engine":"claude","model":"claude-opus-5","cost_usd":0.04}`)

	reply := deliver(t, s, "claude-box", "yes")
	if reply.Kind != tasks.RouteReply || reply.TaskID != route.TaskID {
		t.Fatalf("the reply routed as %+v", reply)
	}

	postJSON(t, s.queryTextHandler, "/api/querytext",
		`{"desktop_id":"claude-box","task_id":"`+route.TaskID+`","content":"DECURIX TASK COMPLETED","kind":"final",
		  "reply":"Paid INV-1042.","cost_usd":0.09,"input_tokens":12000,"output_tokens":800}`)

	rec := postForm(t, s.taskVerdictHandler, "/api/tasks/verdict", "task_id="+route.TaskID+"&verdict=correct")
	if rec.Code != http.StatusOK {
		t.Fatalf("verdict: status %d %s", rec.Code, rec.Body)
	}

	detail, err := s.Tasks.Get(route.TaskID)
	if err != nil {
		t.Fatal(err)
	}

	execution := detail.Executions[0]

	if detail.Status != tasks.StatusDone || detail.Verdict != tasks.VerdictCorrect || detail.Type != tasks.TypeConsequential {
		t.Fatalf("task = %+v", detail.Task)
	}

	if len(execution.Steps) != 1 || !strings.HasPrefix(execution.Steps[0].Action.Before, stepImagePath) {
		t.Fatalf("the on-screen step lost its screenshot: %+v", execution.Steps)
	}

	if len(execution.Interventions) != 1 || execution.Interventions[0].Reply != "yes" || execution.Model != "claude-opus-5" || *execution.CostUSD != 0.09 {
		t.Fatalf("execution = %+v", execution)
	}

	// Every agent entry of the task carries the verdict, on every device.
	thread := readThread(t, s, "claude-box")
	answer := thread.Entries[len(thread.Entries)-1]

	if answer.TaskID != route.TaskID || answer.Verdict != tasks.VerdictCorrect || answer.Kind != conversation.KindEntry {
		t.Fatalf("chat answer = %+v", answer)
	}

	// The report: one settled, non-autonomous task, stepped in on for a payment approval.
	req := httptest.NewRequest(http.MethodGet, "/api/tasks/report?days=7", nil)
	rec = httptest.NewRecorder()
	s.taskReportAPIHandler(rec, req)

	var report tasks.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}

	if report.Totals.Settled != 1 || report.Totals.Autonomous != 0 || len(report.Reasons) != 1 || report.Reasons[0].Key != tasks.ReasonApprovalPayment {
		t.Fatalf("report = %+v", report)
	}

	for _, path := range []string{"/tasks", "/tasks?days=all&engine=claude", "/tasks/" + route.TaskID} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()

		if strings.HasPrefix(req.URL.Path, "/tasks/") {
			s.taskPageHandler(rec, req)
		} else {
			s.tasksPageHandler(rec, req)
		}

		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "pay invoice INV-1042") {
			t.Fatalf("GET %s: status %d", path, rec.Code)
		}
	}
}

func TestVerdictRejectsWhatIsNotOne(t *testing.T) {
	s := chatServer(t)

	route := deliver(t, s, "box", "uname -r")

	cases := map[string]int{
		"task_id=" + route.TaskID + "&verdict=maybe": http.StatusBadRequest,
		"task_id=t0123456789abcdef&verdict=correct":  http.StatusNotFound,
		"task_id=../../etc&verdict=correct":          http.StatusNotFound,
	}

	for body, want := range cases {
		if rec := postForm(t, s.taskVerdictHandler, "/api/tasks/verdict", body); rec.Code != want {
			t.Errorf("%s: status %d, want %d", body, rec.Code, want)
		}
	}
}

// A task ID a desktop reports is only used if it has the shape of one the
// control plane issued; anything else falls back to the desktop's live task.
func TestForgedTaskIDFallsBackToTheLiveTask(t *testing.T) {
	s := chatServer(t)

	route := deliver(t, s, "box", "tidy up /work")

	postJSON(t, s.agentEventHandler, "/api/agent/events",
		`{"desktop_id":"box","task_id":"'; DROP TABLE tasks; --","text":"$ ls /work"}`)

	detail, err := s.Tasks.Get(route.TaskID)
	if err != nil {
		t.Fatal(err)
	}

	if len(detail.Executions[0].Steps) != 1 {
		t.Fatalf("got %d steps", len(detail.Executions[0].Steps))
	}
}

// The trace page and its plain-text twin show everything the store holds for
// a task - steps in order, the question and the answer, the final report -
// and the dashboard's task dropdown API lists the desktop's tasks newest
// first. A task that does not exist, or an ID that is not one, is a 404.
func TestTaskTracePageAndDesktopTasksAPI(t *testing.T) {
	s := chatServer(t)
	s.Web = webui.NewRenderer()

	first := deliver(t, s, "box", "tidy up /work")
	postJSON(t, s.agentEventHandler, "/api/agent/events", `{"desktop_id":"box","task_id":"`+first.TaskID+`","text":"$ ls /work"}`)
	postJSON(t, s.agentEventHandler, "/api/agent/events", `{"desktop_id":"box","task_id":"`+first.TaskID+`","text":"Output: a.txt\nb.txt"}`)
	postJSON(t, s.queryTextHandler, "/api/querytext",
		`{"desktop_id":"box","task_id":"`+first.TaskID+`","content":"DECURIX QUERY REQUEST","kind":"query","reply":"Delete b.txt too?","engine":"claude"}`)
	deliver(t, s, "box", "yes, delete it")
	postJSON(t, s.queryTextHandler, "/api/querytext",
		`{"desktop_id":"box","task_id":"`+first.TaskID+`","content":"DECURIX TASK COMPLETED","kind":"final","reply":"Tidied /work.","input_tokens":100,"output_tokens":20}`)

	second := deliver(t, s, "box", "second request, still running")

	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.taskPageHandler(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	page := get("/tasks/" + first.TaskID + "/trace")
	if page.Code != http.StatusOK {
		t.Fatalf("trace page: status %d %s", page.Code, page.Body)
	}
	for _, want := range []string{"tidy up /work", "$ ls /work", "a.txt\nb.txt", "Delete b.txt too?", "yes, delete it", "Tidied /work.", "100 in / 20 out tokens", "/trace.txt?download=1"} {
		if !strings.Contains(page.Body.String(), want) {
			t.Errorf("the trace page is missing %q", want)
		}
	}

	text := get("/tasks/" + first.TaskID + "/trace.txt")
	if text.Code != http.StatusOK || !strings.HasPrefix(text.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("trace.txt: status %d, type %q", text.Code, text.Header().Get("Content-Type"))
	}
	body := text.Body.String()
	for _, want := range []string{"Request:\ntidy up /work", "[1] ", "Command\n    $ ls /work", "[2] ", "Output\n    Output: a.txt\n    b.txt", "[?] ", "Query asked", "    Delete b.txt too?", "    yes, delete it", "Final report (Done, not checked) --\nTidied /work."} {
		if !strings.Contains(body, want) {
			t.Errorf("trace.txt is missing %q in:\n%s", want, body)
		}
	}
	if strings.Index(body, "[1] ") > strings.Index(body, "[2] ") || strings.Index(body, "[2] ") > strings.Index(body, "[?] ") {
		t.Error("trace.txt is out of order")
	}
	if download := get("/tasks/" + first.TaskID + "/trace.txt?download=1"); !strings.Contains(download.Header().Get("Content-Disposition"), "attachment") {
		t.Error("?download=1 does not download")
	}

	for _, path := range []string{"/tasks/t0123456789abcdef/trace", "/tasks/t0123456789abcdef/trace.txt", "/tasks/not-an-id/trace", "/tasks/" + first.TaskID + "/other", "/tasks/../trace"} {
		if rec := get(path); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", path, rec.Code)
		}
	}

	// The dropdown API: the desktop's tasks newest first with a short title,
	// keyed by desktop; an unknown desktop is an empty list, not an error.
	req := httptest.NewRequest(http.MethodGet, "/api/desktops/tasks?ids=box,nobody", nil)
	rec := httptest.NewRecorder()
	s.desktopTasksHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("desktop tasks: status %d %s", rec.Code, rec.Body)
	}
	var out map[string][]TaskPick
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if got := out["box"]; len(got) != 2 || got[0].ID != second.TaskID || got[1].ID != first.TaskID ||
		got[0].Title != "second request, still running" || got[0].StatusLabel == "" || got[1].Status != tasks.StatusDone {
		t.Fatalf("box's tasks = %+v", got)
	}
	if got, ok := out["nobody"]; !ok || len(got) != 0 {
		t.Fatalf("an unknown desktop's tasks = %+v (listed: %v)", got, ok)
	}
}

// A store failure behind the trace routes is a fixed message, never the
// database's own words.
func TestTaskTraceNeverEchoesTheStoreError(t *testing.T) {
	db := testDB(t)
	store, err := tasks.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	s := &Server{Tasks: store, Web: webui.NewRenderer()}

	for _, path := range []string{"/tasks/t0123456789abcdef/trace", "/tasks/t0123456789abcdef/trace.txt", "/tasks/t0123456789abcdef"} {
		rec := httptest.NewRecorder()
		s.taskPageHandler(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("GET %s with a closed store: status %d, want 500", path, rec.Code)
		}
		if body := rec.Body.String(); strings.Contains(body, "sql") || strings.Contains(body, "closed") || strings.Contains(body, "tasks:") {
			t.Errorf("GET %s echoed the store error: %s", path, body)
		}
	}

	rec := httptest.NewRecorder()
	s.desktopTasksHandler(rec, httptest.NewRequest(http.MethodGet, "/api/desktops/tasks?ids=box", nil))
	if body := rec.Body.String(); rec.Code != http.StatusInternalServerError || strings.Contains(body, "sql") || strings.Contains(body, "closed") {
		t.Errorf("desktop tasks with a closed store: status %d body %s", rec.Code, body)
	}
}
