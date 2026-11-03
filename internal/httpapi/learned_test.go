package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// postLearned posts a learned row as desktop from (requestDesktop), or from the host when from is "".
func postLearned(t *testing.T, s *Server, from string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/desktops/learned", strings.NewReader(string(raw)))
	if from != "" {
		req = req.WithContext(context.WithValue(req.Context(), callerKey{}, from))
	}
	rec := httptest.NewRecorder()
	s.desktopLearnedHandler(rec, req)
	return rec
}

// What a desktop keeps from a person's reply is kept here under that person, citing their judgement, and it outlives
// the desktop (R8, C7); a citation that is not a person's judgement on the desktop's own task is refused.
func TestALearnedRowIsKeptUnderThePersonWhoSaidIt(t *testing.T) {
	s := chatServer(t)
	// A person answers box's question: that answer is the judgement a correction cites.
	reply := tasks.Route{}
	route, _ := s.Tasks.Commit(tasks.NewRoute(), tasks.Worker{ID: "box", Name: "box"}, "enter the bills")
	_, _ = s.Tasks.RecordOutcome("box", route.TaskID, tasks.Outcome{Kind: tasks.OutcomeQuery, Reply: "Which ledger?"})
	reply, _ = s.Tasks.Plan("box", "purchases, always")
	asha := tasks.Actor{ID: "u_asha", Name: "Asha", Roles: []string{"operator"}, Kind: tasks.ActorPerson}
	reply, _, err := s.Tasks.CommitReply(reply, tasks.Worker{ID: "box", Name: "box"}, "purchases, always", false,
		tasks.ReplyBy{Actor: asha, Door: "chat"})
	if err != nil || reply.JudgementID == "" {
		t.Fatalf("reply = %+v, %v", reply, err)
	}
	// A worker's judgement on the same task (it cannot author a lesson).
	workerJ, _ := s.Tasks.AddJudgement(tasks.Judgement{Actor: tasks.Actor{ID: "eve", Kind: tasks.ActorWorker},
		Kind: tasks.JudgementAnswer, WorkerID: "box", TaskID: route.TaskID, Text: "x"})
	// Another desktop's task and a person's answer on it.
	other, _ := s.Tasks.Commit(tasks.NewRoute(), tasks.Worker{ID: "eve", Name: "eve"}, "something of eve's")

	body := func(task, from string) map[string]any {
		return map[string]any{"task_id": task, "kind": "correction", "skill": "bill-entry", "skill_version": 2,
			"derived_from": from, "question": "Which ledger?", "text": "Bills go to the purchases ledger."}
	}

	rec := postLearned(t, s, "box", body(route.TaskID, reply.JudgementID))
	if rec.Code != http.StatusCreated {
		t.Fatalf("a correction: %d %s", rec.Code, rec.Body)
	}
	var created struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	j, ok, _ := s.Tasks.Judgement(created.ID)
	if !ok || j.Actor.ID != "u_asha" || j.Actor.Roles[0] != "operator" || j.Kind != tasks.JudgementCorrection ||
		j.Door != "derived" || j.DerivedFrom != reply.JudgementID || j.SkillName != "bill-entry" || j.SkillVersion != 2 {
		t.Fatalf("kept as %+v", j)
	}

	for name, c := range map[string]struct {
		from string
		body map[string]any
		want int
	}{
		"an unknown judgement":      {"box", body(route.TaskID, tasks.NewJudgementID()), http.StatusConflict},
		"a worker's judgement":      {"box", body(route.TaskID, workerJ), http.StatusConflict},
		"another desktop's task":    {"box", body(other.TaskID, reply.JudgementID), http.StatusForbidden},
		"from the host":             {"", body(route.TaskID, reply.JudgementID), http.StatusForbidden},
		"another desktop's posting": {"eve", body(route.TaskID, reply.JudgementID), http.StatusForbidden},
		"a kind no desktop keeps":   {"box", map[string]any{"task_id": route.TaskID, "kind": "verdict", "derived_from": reply.JudgementID, "text": "x"}, http.StatusBadRequest},
	} {
		if rec := postLearned(t, s, c.from, c.body); rec.Code != c.want {
			t.Errorf("%s: %d %s, want %d", name, rec.Code, rec.Body, c.want)
		}
	}

	// The desktop is deleted (its worker record marked so) and made again under the same name: the row is served.
	if err := s.Tasks.MarkWorkerDeleted("box"); err != nil {
		t.Fatal(err)
	}
	if err := s.Tasks.UpsertWorker(tasks.Worker{ID: "box", Name: "box"}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.judgementsHandler(w, httptest.NewRequest(http.MethodGet, "/api/judgements?worker=box", nil))
	if !strings.Contains(w.Body.String(), created.ID) || !strings.Contains(w.Body.String(), "Bills go to the purchases ledger.") {
		t.Fatalf("after the desktop was recreated: %s", w.Body)
	}
}
