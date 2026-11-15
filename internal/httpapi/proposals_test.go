package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/knowledge"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// proposalServer is a server with tasks and knowledge whose primary listener is guarded, box-a calling back from
// 172.30.0.5 and box-b from 172.30.0.6; and one task of box-a's with a person's answer on it.
func proposalServer(t *testing.T) (*Server, http.Handler, string) {
	t.Helper()
	s := chatServer(t)
	var err error
	if s.Knowledge, err = knowledge.NewStore(testDB(t)); err != nil {
		t.Fatal(err)
	}
	s.netsFor = staticNets("172.30.0.0/16", "172.30.0.1")
	s.callerAt = func(r *http.Request) (string, bool, error) {
		switch {
		case strings.HasPrefix(r.RemoteAddr, "172.30.0.5:"):
			return "box-a", true, nil
		case strings.HasPrefix(r.RemoteAddr, "172.30.0.6:"):
			return "box-b", true, nil
		}
		return "", false, nil
	}
	mux := http.NewServeMux()
	s.RegisterLocalRoutes(mux)

	route, err := s.Tasks.Commit(tasks.NewRoute(), tasks.Worker{ID: "box-a", Name: "box-a"}, "send the weekly summary",
		tasks.Origin{Kind: tasks.OriginPerson})
	if err != nil {
		t.Fatal(err)
	}
	asha := tasks.Actor{ID: "u_asha", Name: "Asha", Roles: []string{"operator"}, Kind: tasks.ActorPerson}
	if _, err := s.Tasks.AddJudgement(tasks.Judgement{Actor: asha, Kind: tasks.JudgementAnswer, WorkerID: "box-a",
		TaskID: route.TaskID, Text: "Always put the region column first."}); err != nil {
		t.Fatal(err)
	}
	return s, s.GuardPrimary(mux), route.TaskID
}

// do sends method path with body (JSON, or none) from remote through h.
func do(h http.Handler, method, path, remote string, body any) *httptest.ResponseRecorder {
	var raw string
	if body != nil {
		b, _ := json.Marshal(body)
		raw = string(b)
	}
	r := httptest.NewRequest(method, path, strings.NewReader(raw))
	r.RemoteAddr = remote
	r.Host = "127.0.0.1:8080"
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

const (
	fromBoxA = "172.30.0.5:40000"
	fromBoxB = "172.30.0.6:40000"
)

func lesson(text, quote, where string) map[string]any {
	return map[string]any{"kind": "lesson", "text": text, "why": "the person said so",
		"evidence": []map[string]string{{"quote": quote, "where": where}}}
}

func TestADesktopProposesForItsOwnTaskAndTheCodeChecksEachProposal(t *testing.T) {
	s, h, task := proposalServer(t)
	s.secretsSeen.Remember("box-a", "Zq81-secret-value")
	good := lesson("The region column comes first in the weekly summary.", "put the region column first", "person")
	body := map[string]any{"task_id": task, "model": "m", "proposals": []map[string]any{
		good,
		lesson("Summaries are sent on Mondays.", "send it every Monday", "person"),      // not the person's words
		lesson("The upload key is Zq81-secret-value.", "region column first", "person"), // a value marked secret
	}}
	rec := do(h, http.MethodPost, "/api/desktops/proposals", fromBoxA, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("propose: %d %s", rec.Code, rec.Body)
	}
	var answer struct {
		Kept    []map[string]any  `json:"kept"`
		Refused []proposalRefused `json:"refused"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &answer)
	if len(answer.Kept) != 1 || len(answer.Refused) != 2 || answer.Refused[0].Index != 1 || answer.Refused[1].Index != 2 {
		t.Fatalf("answer = %s", rec.Body)
	}
	// Shaped like a secret; and the same text again is a duplicate.
	rec = do(h, http.MethodPost, "/api/desktops/proposals", fromBoxA, map[string]any{"task_id": task, "proposals": []map[string]any{
		lesson("Sign in with password: hunter2-Xy9.", "Always put the region", "person"), good}})
	var again struct {
		Kept    []map[string]any  `json:"kept"`
		Refused []proposalRefused `json:"refused"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &again)
	if len(again.Kept) != 0 || len(again.Refused) != 2 {
		t.Fatalf("second post = %s", rec.Body)
	}
	// More than a run proposes is refused whole.
	four := []map[string]any{good, good, good, good}
	if got := do(h, http.MethodPost, "/api/desktops/proposals", fromBoxA, map[string]any{"task_id": task, "proposals": four}); got.Code != http.StatusBadRequest {
		t.Errorf("four proposals: %d", got.Code)
	}

	// Another desktop's task, a desktop claiming nothing, and the person's routes from a desktop.
	if got := do(h, http.MethodPost, "/api/desktops/proposals", fromBoxB, map[string]any{"task_id": task,
		"proposals": []map[string]any{lesson("x", "region", "run")}}); got.Code != http.StatusForbidden {
		t.Errorf("another desktop's task: %d", got.Code)
	}
	if got := do(h, http.MethodPost, "/api/desktops/proposals", fromHost, map[string]any{"task_id": task,
		"proposals": []map[string]any{lesson("x", "region", "run")}}); got.Code != http.StatusForbidden {
		t.Errorf("from the host: %d", got.Code)
	}
	for _, path := range []string{"/api/knowledge/proposals/approve", "/api/knowledge/proposals/reject"} {
		if got := do(h, http.MethodPost, path, fromBoxA, map[string]any{"id": answer.Kept[0]["id"]}); got.Code != http.StatusForbidden {
			t.Errorf("%s from a desktop: %d", path, got.Code)
		}
	}
	if got := do(h, http.MethodGet, "/api/knowledge/proposals", fromBoxA, nil); got.Code != http.StatusForbidden {
		t.Errorf("the list from a desktop: %d", got.Code)
	}
	if !containerMayCall(http.MethodPost, "/api/desktops/proposals") || containerMayCall(http.MethodPost, "/api/knowledge/proposals/approve") {
		t.Fatal("the allowlist does not hold the proposals callback, or holds the approval")
	}
	// While proposed it is given to no one.
	if items, _ := s.Knowledge.ForWorker("box-a", nil); len(items) != 0 {
		t.Fatalf("a proposal was given out: %+v", items)
	}
}

func TestAPersonApprovesOrRejectsAndIsTheAuthor(t *testing.T) {
	s, h, task := proposalServer(t)
	rec := do(h, http.MethodPost, "/api/desktops/proposals", fromBoxA, map[string]any{"task_id": task, "proposals": []map[string]any{
		lesson("The region column comes first in the weekly summary.", "put the region column first", "person"),
		lesson("The weekly summary is a spreadsheet.", "office read summary.xlsx", "run"),
	}})
	var answer struct {
		Kept []struct {
			ID string `json:"id"`
		} `json:"kept"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &answer)
	if len(answer.Kept) != 2 {
		t.Fatalf("propose: %s", rec.Body)
	}

	list := do(h, http.MethodGet, "/api/knowledge/proposals", fromHost, nil)
	var listed struct {
		Proposals []knowledge.Proposal `json:"proposals"`
		Requests  map[string]string    `json:"requests"`
	}
	_ = json.Unmarshal(list.Body.Bytes(), &listed)
	if list.Code != http.StatusOK || len(listed.Proposals) != 2 || listed.Requests[task] != "send the weekly summary" {
		t.Fatalf("list: %d %s", list.Code, list.Body)
	}

	approved := do(h, http.MethodPost, "/api/knowledge/proposals/approve", fromHost, map[string]any{"id": answer.Kept[0].ID})
	if approved.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", approved.Code, approved.Body)
	}
	items, _ := s.Knowledge.ForWorker("box-a", nil)
	if len(items) != 1 || items[0].Source.Kind != knowledge.SourcePerson || items[0].Source.Ref != tasks.LocalOwner.ID ||
		items[0].Source.TaskID != task {
		t.Fatalf("given = %+v", items)
	}
	if got := do(h, http.MethodPost, "/api/knowledge/proposals/reject", fromHost, map[string]any{"id": answer.Kept[1].ID, "note": "a one-off"}); got.Code != http.StatusOK {
		t.Fatalf("reject: %d %s", got.Code, got.Body)
	}
	if got := do(h, http.MethodPost, "/api/knowledge/proposals/approve", fromHost, map[string]any{"id": answer.Kept[1].ID}); got.Code != http.StatusBadRequest {
		t.Fatalf("approving a rejected proposal: %d", got.Code)
	}

	// Both decisions are the local owner's judgements on the run's task (plan R5).
	rows, _ := s.Tasks.JudgementsForTask(task)
	decided := map[string]bool{}
	for _, j := range rows {
		if j.Kind == tasks.JudgementKnowledge && j.Actor.ID == tasks.LocalOwner.ID && j.WorkerID == "box-a" {
			var p struct{ Decision string }
			_ = json.Unmarshal(j.Payload, &p)
			decided[p.Decision] = true
		}
	}
	if !decided[knowledge.StateApproved] || !decided[knowledge.StateRejected] {
		t.Fatalf("decisions kept as judgements: %v", decided)
	}
	all := do(h, http.MethodGet, "/api/knowledge/proposals?state=all", fromHost, nil)
	_ = json.Unmarshal(all.Body.Bytes(), &listed)
	if len(listed.Proposals) != 2 {
		t.Fatalf("a decided proposal is gone: %s", all.Body)
	}
}

func TestAnApprovalOfAChangedItemIsRefused(t *testing.T) {
	s, h, task := proposalServer(t)
	old, err := s.Knowledge.Add(knowledge.Item{Scope: knowledge.ScopeWorker, ScopeKey: "box-a", Kind: knowledge.KindLesson,
		Text: "The region column comes last.", Source: knowledge.Source{Kind: knowledge.SourcePerson, Ref: "owner"}})
	if err != nil {
		t.Fatal(err)
	}
	p := lesson("The region column comes first in the weekly summary.", "put the region column first", "person")
	p["replaces"] = "The region column comes last."
	rec := do(h, http.MethodPost, "/api/desktops/proposals", fromBoxA, map[string]any{"task_id": task, "proposals": []map[string]any{p}})
	var answer struct {
		Kept []struct {
			ID string `json:"id"`
		} `json:"kept"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &answer)
	if len(answer.Kept) != 1 {
		t.Fatalf("propose: %s", rec.Body)
	}
	if err := s.Knowledge.Retire(old.ID); err != nil {
		t.Fatal(err)
	}
	got := do(h, http.MethodPost, "/api/knowledge/proposals/approve", fromHost, map[string]any{"id": answer.Kept[0].ID})
	if got.Code != http.StatusConflict || !strings.Contains(got.Body.String(), "it changed since this was proposed") {
		t.Fatalf("approve after the item changed: %d %s", got.Code, got.Body)
	}
}
