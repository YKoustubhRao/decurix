package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/concierge"
	"github.com/YKoustubhRao/decurix/internal/conversation"
	"github.com/YKoustubhRao/decurix/internal/querynotes"
	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
	"github.com/YKoustubhRao/decurix/internal/tasks"
	"github.com/YKoustubhRao/decurix/internal/webui"
	"github.com/YKoustubhRao/decurix/internal/withhold"
)

// A final report the desktop's check did not judge is shown as "not checked" on the task page and in the finished
// feed, and its trace renders whole (W-01, W-02, W-10).
func TestAnUncheckedFinalIsShownAsNotChecked(t *testing.T) {
	s := chatServer(t)
	s.Web = webui.NewRenderer()
	r := deliver(t, s, "box", "tidy up /work")

	// The feed's first call only says where "now" is.
	feed := httptest.NewRecorder()
	s.finishedTasksHandler(feed, httptest.NewRequest(http.MethodGet, "/api/tasks/finished", nil))

	postJSON(t, s.agentEventHandler, "/api/agent/events", `{"desktop_id":"box","task_id":"`+r.TaskID+`","text":"$ ls /work"}`)
	if rec := postJSON(t, s.queryTextHandler, "/api/querytext",
		`{"desktop_id":"box","task_id":"`+r.TaskID+`","content":"done","kind":"final","reply":"FINAL-MARK","checked":false}`); rec.Code != http.StatusOK {
		t.Fatalf("querytext: %d %s", rec.Code, rec.Body)
	}

	page := httptest.NewRecorder()
	s.taskPageHandler(page, httptest.NewRequest(http.MethodGet, "/tasks/"+r.TaskID, nil))
	if !strings.Contains(page.Body.String(), "Done, not checked") {
		t.Error("the task page does not say the result was not checked")
	}

	trace := httptest.NewRecorder()
	s.taskPageHandler(trace, httptest.NewRequest(http.MethodGet, "/tasks/"+r.TaskID+"/trace", nil))
	body := trace.Body.String()
	if trace.Code != http.StatusOK || strings.Contains(body, "internal server error") ||
		!strings.Contains(body, "FINAL-MARK") || !strings.Contains(body, "Final check: not run") {
		t.Fatalf("trace: status %d, whole=%v", trace.Code, !strings.Contains(body, "internal server error"))
	}

	var latest struct {
		Items []FinishedTask `json:"items"`
	}
	rec := httptest.NewRecorder()
	s.finishedTasksHandler(rec, httptest.NewRequest(http.MethodGet, "/api/tasks/finished?after=0", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &latest); err != nil || len(latest.Items) != 1 {
		t.Fatalf("finished feed: %s (%v)", rec.Body, err)
	}
	if item := latest.Items[0]; !strings.Contains(item.Phrase, "not checked") || item.Checked == nil || *item.Checked {
		t.Fatalf("finished item = %+v, want it said not checked", item)
	}
}

// A kind this control plane does not know is a question in the chat as in the records - never a plain answer (W-06).
func TestAnUnknownKindIsAQuestionEverywhere(t *testing.T) {
	s := chatServer(t)
	r := deliver(t, s, "box", "do the thing")
	postJSON(t, s.queryTextHandler, "/api/querytext",
		`{"desktop_id":"box","task_id":"`+r.TaskID+`","content":"I could not confirm the totals","kind":"unverified","reply":"I could not confirm the totals"}`)

	d, _ := s.Tasks.Get(r.TaskID)
	entries := s.Chat.Snapshot("box").Entries
	if d.Status != tasks.StatusWaiting || entries[len(entries)-1].Kind != conversation.KindQuery {
		t.Fatalf("task %s, chat kind %q; want waiting and a query", d.Status, entries[len(entries)-1].Kind)
	}
}

// An outcome for a task that already ended is refused with a 4xx (the agent then lets it go) and lands nowhere: the
// live task is untouched and the chat shows nothing of it (C-01, L-03).
func TestAStaleOutcomeIsRefusedAndTouchesNothing(t *testing.T) {
	s := chatServer(t)
	a := deliver(t, s, "box", "task A")
	if err := s.Tasks.CloseOpen("box", "stopped"); err != nil {
		t.Fatal(err)
	}
	b := deliver(t, s, "box", "task B")
	before := len(s.Chat.Snapshot("box").Entries)

	rec := postJSON(t, s.queryTextHandler, "/api/querytext",
		`{"desktop_id":"box","task_id":"`+a.TaskID+`","content":"A done","kind":"final","reply":"A is done","checked":true}`)
	if rec.Code < 400 || rec.Code >= 500 {
		t.Fatalf("stale outcome answered %d, want a refusal (4xx)", rec.Code)
	}
	if d, _ := s.Tasks.Get(b.TaskID); d.Status != tasks.StatusRunning {
		t.Fatalf("task B is %s, want still running", d.Status)
	}
	if after := len(s.Chat.Snapshot("box").Entries); after != before {
		t.Fatalf("the refused outcome reached the chat (%d -> %d entries)", before, after)
	}
}

// A question the desktop asked for a secret has its reply kept as withhold.Secret in the task store, the chat and
// what the send answers - even a bare word no list would catch (X-6, C-03, C-05).
func TestAReplyToASecretQuestionIsKeptNowhere(t *testing.T) {
	s := chatServer(t)
	r := deliver(t, s, "box", "log in to the portal")
	postJSON(t, s.queryTextHandler, "/api/querytext",
		`{"desktop_id":"box","task_id":"`+r.TaskID+`","content":"What is the password?","kind":"query","reply":"What is the password?","secret":true}`)

	route, stored := s.recordDelivered(context.Background(), "box", "Sunshine", s.routeRequest("box", "Sunshine", tasks.Origin{Kind: tasks.OriginPerson}))
	if route.Kind != tasks.RouteReply || stored != withhold.Secret {
		t.Fatalf("route %s, stored %q", route.Kind, stored)
	}
	d, _ := s.Tasks.Get(r.TaskID)
	if reply := d.Executions[0].Interventions[0].Reply; reply != withhold.Secret {
		t.Fatalf("intervention reply %q", reply)
	}
	for _, e := range s.Chat.Snapshot("box").Entries {
		if strings.Contains(e.Content, "Sunshine") {
			t.Fatalf("the chat kept %q", e.Content)
		}
	}
}

// What a person marked as a secret (the compose box's toggle) is kept as withhold.Secret everywhere, while the text
// handed on for delivery is what they typed.
func TestASendMarkedSecretIsStoredWithheld(t *testing.T) {
	s := chatServer(t)
	if err := s.Tasks.UpsertWorker(tasks.Worker{ID: "box", Engine: "thebot"}); err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), sentSecretKey{}, true)
	route, stored := s.recordDelivered(ctx, "box", "the code is plainword", s.routeRequest("box", "the code is plainword", tasks.Origin{Kind: tasks.OriginPerson}))
	if stored != withhold.Secret {
		t.Fatalf("stored %q", stored)
	}
	if d, _ := s.Tasks.Get(route.TaskID); d.Request != withhold.Secret {
		t.Fatalf("task request %q", d.Request)
	}

}

// The concierge's Approve button answers only with the approval's note: then the reply is bound to its gate request;
// without it, nothing is sent and the person is pointed to the card (W-03).
func TestTheConciergeApproveButtonIsBoundToItsNote(t *testing.T) {
	s := chatServer(t)
	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store, err := concierge.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}

	var boundTo []string
	deps := concierge.Deps{
		Transcript: func(id string) conversation.Thread { return s.Chat.Snapshot(id) },
		Send: func(ctx context.Context, id, text string) (string, string, error) {
			_, _, request, err := s.replyBinding(ctx, id)
			if err != nil {
				return "", "", err
			}
			boundTo = append(boundTo, request)
			return "t1", tasks.RouteReply, nil
		},
	}
	s.Concierge = concierge.New(store, deps)

	sess, err := store.CreateSession("box", "box", "t1", "pay the vendor")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSession(sess.ID, concierge.StatusNeedsYou, conversation.KindApproval, "e1"); err != nil {
		t.Fatal(err)
	}
	s.QueryNotes.Add(querynotes.QueryNote{ID: "n1", DesktopID: "box", Kind: querynotes.KindApproval, Content: "pay?",
		GateRequest: "r1", GateQuestion: "Approval needed before I click Pay.", CreatedAt: time.Now()})

	post := func(form url.Values) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/concierge/answer", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		s.conciergeAnswerHandler(rec, req)
		return rec
	}

	for _, form := range []url.Values{
		{"session": {"1"}, "answer": {"yes"}},
		{"session": {"1"}, "answer": {"yes"}, "note_id": {"someone-else"}},
	} {
		rec := post(form)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "card") || len(boundTo) != 0 {
			t.Fatalf("%v: %d %s, sent %v", form, rec.Code, rec.Body, boundTo)
		}
	}

	rec := post(url.Values{"session": {"1"}, "answer": {"yes"}, "note_id": {"n1"}})
	if rec.Code != http.StatusOK || len(boundTo) != 1 || boundTo[0] != "r1" {
		t.Fatalf("bound answer: %d %s, bound to %v", rec.Code, rec.Body, boundTo)
	}
}
