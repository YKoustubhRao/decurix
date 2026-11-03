package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/access"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// askedQuestion leaves a task on box waiting on a question of kind (query or approval) and returns its id.
func askedQuestion(t *testing.T, s *Server, kind, question string) string {
	t.Helper()
	w := tasks.Worker{ID: "box", Name: "box", Engine: "thebot"}
	route, err := s.Tasks.Commit(tasks.NewRoute(), w, "file the bills", tasks.Origin{Kind: tasks.OriginPerson})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Tasks.RecordOutcome("box", route.TaskID, tasks.Outcome{Kind: kind, Reply: question}); err != nil {
		t.Fatal(err)
	}
	return route.TaskID
}

// sendAs posts form to /api/desktops/send from the host, through the gated listener's marking when gated, with the
// sign-in cookie when given.
func sendAs(t *testing.T, s *Server, form url.Values, gated bool, cookie string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/desktops/send", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "127.0.0.1:40000"
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: access.CookieName, Value: cookie})
	}
	rec := httptest.NewRecorder()
	if gated {
		MarkGated(http.HandlerFunc(s.sendRequestHandler)).ServeHTTP(rec, req)
	} else {
		s.sendRequestHandler(rec, req)
	}
	return rec
}

// requestsWith is the bodies the fake agent got that carry field: its /request bodies.
func requestsWith(bodies []map[string]any, field string) []map[string]any {
	var out []map[string]any
	for _, b := range bodies {
		if _, ok := b[field]; ok {
			out = append(out, b)
		}
	}
	return out
}

func onlyJudgement(t *testing.T, s *Server, taskID string) tasks.Judgement {
	t.Helper()
	list, err := s.Tasks.JudgementsForTask(taskID)
	if err != nil || len(list) != 1 {
		t.Fatalf("judgements of %s = %+v, %v", taskID, list, err)
	}
	return list[0]
}

// A reply typed in the chat on this computer is the local owner's judgement.
func TestAChatReplyIsTheLocalOwnersJudgement(t *testing.T) {
	s := chatServer(t)
	bodies := fakeAgentDesktop(t, s)
	task := askedQuestion(t, s, tasks.OutcomeQuery, "Which ledger?")
	if rec := sendAs(t, s, url.Values{"id": {"box"}, "content": {"the purchases ledger"}}, false, ""); rec.Code != http.StatusOK {
		t.Fatalf("reply: %d %s", rec.Code, rec.Body)
	}
	j := onlyJudgement(t, s, task)
	if j.Actor.ID != tasks.LocalOwner.ID || j.Actor.Kind != tasks.ActorPerson || j.Kind != tasks.JudgementAnswer || j.Door != "chat" {
		t.Fatalf("judgement = %+v", j)
	}
	d, _ := s.Tasks.Get(task)
	if iv := d.Executions[0].Interventions[0]; iv.ReplierID != tasks.LocalOwner.ID || iv.JudgementID != j.ID {
		t.Fatalf("intervention = %+v", iv)
	}
	if got := requestsWith(bodies(), "content"); len(got) != 1 {
		t.Fatalf("the agent got %d requests: %v", len(got), bodies())
	}
}

// A reply through the gated listener is the signed-in person's, with their role; without a sign-in it is someone
// holding the access code's, with no role.
func TestAGatedReplyIsTheSignedInPersonsJudgement(t *testing.T) {
	s, store := accessServer(t)
	fakeAgentDesktop(t, s)
	asha, err := store.CreateUser("asha", "password-1", []string{"operator"})
	if err != nil {
		t.Fatal(err)
	}
	cookie, _, err := store.NewSession(asha.ID)
	if err != nil {
		t.Fatal(err)
	}
	task := askedQuestion(t, s, tasks.OutcomeQuery, "Which ledger?")
	if rec := sendAs(t, s, url.Values{"id": {"box"}, "content": {"purchases"}}, true, cookie); rec.Code != http.StatusOK {
		t.Fatalf("reply: %d %s", rec.Code, rec.Body)
	}
	j := onlyJudgement(t, s, task)
	if j.Actor.ID != asha.ID || len(j.Actor.Roles) != 1 || j.Actor.Roles[0] != "operator" || j.Door != "phone" {
		t.Fatalf("judgement = %+v", j)
	}

	// Answered, so the task runs again; a second question is answered by someone not signed in.
	if _, err := s.Tasks.RecordOutcome("box", task, tasks.Outcome{Kind: tasks.OutcomeQuery, Reply: "Which month?"}); err != nil {
		t.Fatal(err)
	}
	if rec := sendAs(t, s, url.Values{"id": {"box"}, "content": {"October"}}, true, ""); rec.Code != http.StatusOK {
		t.Fatalf("reply: %d %s", rec.Code, rec.Body)
	}
	list, _ := s.Tasks.JudgementsForTask(task)
	if len(list) != 2 || list[1].Actor.ID != accessCodeHolder.ID || len(list[1].Actor.Roles) != 0 {
		t.Fatalf("judgements = %+v", list)
	}
}

// An approval answered from the concierge's buttons is an approval judgement naming the access log's entry.
func TestAConciergeApprovalIsAnApprovalJudgement(t *testing.T) {
	s, _ := accessServer(t)
	fakeAgentDesktop(t, s)
	task := askedQuestion(t, s, tasks.OutcomeApproval, "Approval needed before I click Pay: pays 900.")
	// The concierge's Approve button sends through ConciergeDeps().Send with the person's context and its door,
	// as conciergeAnswerHandler builds it.
	req := httptest.NewRequest(http.MethodPost, "/api/concierge/answer", nil)
	req.RemoteAddr = "127.0.0.1:40000"
	ctx := withDoor(s.ActorContext(req), "concierge")
	if _, _, err := s.ConciergeDeps().Send(ctx, "box", "yes"); err != nil {
		t.Fatal(err)
	}
	j := onlyJudgement(t, s, task)
	var payload map[string]string
	_ = json.Unmarshal(j.Payload, &payload)
	if j.Kind != tasks.JudgementApproval || j.Actor.ID != access.LocalOwnerID || j.Door != "concierge" ||
		!strings.HasPrefix(payload["approval_id"], "a_") || payload["outcome"] != access.OutcomeAllowed {
		t.Fatalf("judgement = %+v (%s)", j, j.Payload)
	}
}

// A reply with nobody to author it (a desktop's own call, an automated door) is refused before it reaches the
// desktop, and leaves no judgement.
func TestAReplyWithNoActorIsRefused(t *testing.T) {
	s := chatServer(t)
	bodies := fakeAgentDesktop(t, s)
	task := askedQuestion(t, s, tasks.OutcomeQuery, "Which ledger?")
	_, _, err := s.submitStored(context.Background(), "box", "purchases", tasks.Origin{Kind: tasks.OriginPerson})
	var refused *errUpload
	if err == nil || !errors.As(err, &refused) || refused.status != http.StatusForbidden {
		t.Fatalf("a reply with no actor = %v", err)
	}
	if len(requestsWith(bodies(), "content")) != 0 {
		t.Fatal("the reply reached the desktop")
	}
	if list, _ := s.Tasks.JudgementsForTask(task); len(list) != 0 {
		t.Fatalf("judgements = %+v", list)
	}
	// From a desktop's own address, the compose door gives it no actor either.
	req := httptest.NewRequest(http.MethodPost, "/api/desktops/send",
		strings.NewReader(url.Values{"id": {"box"}, "content": {"purchases"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "172.18.0.7:40000"
	s.netsFor = staticNets("172.18.0.0/16", "172.18.0.1")
	rec := httptest.NewRecorder()
	s.sendRequestHandler(rec, req)
	if rec.Code != http.StatusForbidden || len(requestsWith(bodies(), "content")) != 0 {
		t.Fatalf("a desktop's own reply: %d %s", rec.Code, rec.Body)
	}
}

// A verdict is stored with who gave it; a desktop cannot give one; the judgements outlive their worker.
func TestAVerdictHasItsAuthorAndOutlivesTheWorker(t *testing.T) {
	s := chatServer(t)
	route, _ := s.Tasks.Commit(tasks.NewRoute(), tasks.Worker{ID: "box", Name: "box"}, "count the bills", tasks.Origin{Kind: tasks.OriginPerson})
	_, _ = s.Tasks.RecordOutcome("box", route.TaskID, tasks.Outcome{Kind: tasks.OutcomeDone, Reply: "12"})
	verdict := func(remote string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/tasks/verdict",
			strings.NewReader(url.Values{"task_id": {route.TaskID}, "verdict": {"wrong"}, "note": {"13"}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		s.taskVerdictHandler(rec, req)
		return rec
	}
	s.netsFor = staticNets("172.18.0.0/16", "172.18.0.1")
	if rec := verdict("172.18.0.7:1"); rec.Code != http.StatusForbidden {
		t.Fatalf("a desktop's verdict: %d", rec.Code)
	}
	if rec := verdict("127.0.0.1:1"); rec.Code != http.StatusOK {
		t.Fatalf("a person's verdict: %d %s", rec.Code, rec.Body)
	}
	j := onlyJudgement(t, s, route.TaskID)
	if j.Kind != tasks.JudgementVerdict || j.Actor.ID != tasks.LocalOwner.ID {
		t.Fatalf("judgement = %+v", j)
	}
	if err := s.Tasks.MarkWorkerDeleted("box"); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.judgementsHandler(rec, httptest.NewRequest(http.MethodGet, "/api/judgements?worker=box", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), j.ID) {
		t.Fatalf("a deleted worker's judgements: %d %s", rec.Code, rec.Body)
	}
}

// A person's reply reaches the desktop naming the judgement it is (C6), and every request carries the date order
// when the control plane has one.
func TestAReplyTellsTheDesktopItsJudgement(t *testing.T) {
	s := chatServer(t)
	s.DateOrder = "month"
	bodies := fakeAgentDesktop(t, s)
	task := askedQuestion(t, s, tasks.OutcomeQuery, "Which ledger?")
	if rec := sendAs(t, s, url.Values{"id": {"box"}, "content": {"purchases"}}, false, ""); rec.Code != http.StatusOK {
		t.Fatalf("reply: %d %s", rec.Code, rec.Body)
	}
	j := onlyJudgement(t, s, task)
	got := requestsWith(bodies(), "content")
	if len(got) != 1 || got[0]["judgement_id"] != j.ID || got[0]["date_order"] != "month" {
		t.Fatalf("the desktop got %v, want judgement %s", got, j.ID)
	}
	// It names the execution it belongs to (H-PY-7): the attempt waiting on this answer.
	if got[0]["execution_id"] != j.ExecutionID || j.ExecutionID == "" {
		t.Fatalf("the reply names execution %v, want %s", got[0]["execution_id"], j.ExecutionID)
	}
}
