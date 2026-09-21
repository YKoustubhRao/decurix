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

	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

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

	return s.recordDelivered(context.Background(), desktopID, content, s.routeRequest(desktopID, content))
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
