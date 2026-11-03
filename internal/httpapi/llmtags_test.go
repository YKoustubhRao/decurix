package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/llmrec"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// The proxy stores what each call says it is for, flags what is missing or malformed, and with the switch on refuses
// an untagged call (C1, D3).
func TestTheProxyStoresAndChecksCallTags(t *testing.T) {
	s := recordingServer(t)
	route := tasks.NewRoute()
	if _, err := s.Tasks.Commit(route, tasks.Worker{ID: "box", Name: "box"}, "file the bills"); err != nil {
		t.Fatal(err)
	}
	other := tasks.NewRoute()
	if _, err := s.Tasks.Commit(other, tasks.Worker{ID: "eve", Name: "eve"}, "something of eve's"); err != nil {
		t.Fatal(err)
	}
	body := `{"messages":[{"role":"user","content":"hi"}]}`
	call := func(task string, headers map[string]string) llmrec.Row {
		t.Helper()
		headers[llmrec.HeaderTask] = task
		if rec := proxyCall(t, s, "azure", "box", body, headers); rec.Code != http.StatusOK {
			t.Fatalf("call: %d %s", rec.Code, rec.Body)
		}
		rows, _ := s.LLMRec.Calls(task)
		return rows[len(rows)-1]
	}

	r := call(route.TaskID, map[string]string{llmrec.HeaderPurpose: "review", llmrec.HeaderTurn: "j1:4", llmrec.HeaderCaller: "agent"})
	if r.Purpose != "review" || r.Turn != "j1:4" || r.Caller != "agent" || len(r.Problems) != 0 {
		t.Fatalf("a well-tagged call = %+v", r)
	}
	r = call(route.TaskID, map[string]string{})
	if r.Purpose != llmrec.PurposeUntagged || strings.Join(r.Problems, ",") != "untagged" {
		t.Fatalf("an untagged call = %+v", r)
	}
	r = call(route.TaskID, map[string]string{llmrec.HeaderPurpose: "Review Now", llmrec.HeaderTurn: "j1 4; drop"})
	if r.Purpose != llmrec.PurposeInvalid || r.Turn != llmrec.PurposeInvalid ||
		strings.Join(r.Problems, ",") != "invalid purpose,invalid turn" {
		t.Fatalf("a malformed call = %+v", r)
	}
	r = call(other.TaskID, map[string]string{llmrec.HeaderPurpose: "review"})
	if r.DesktopID != "box" || strings.Join(r.Problems, ",") != "task not this desktop's" {
		t.Fatalf("another desktop's task = %+v", r)
	}

	st, _ := s.LLMRec.Stats()
	if st.Untagged != 1 || st.Invalid != 1 || st.ByPurpose["review"] != 2 {
		t.Fatalf("stats = %+v", st)
	}

	s.RequirePurpose = true
	rec := proxyCall(t, s, "azure", "box", body, map[string]string{llmrec.HeaderTask: route.TaskID})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "X-Decurix-Purpose") {
		t.Fatalf("untagged with the switch on: %d %s", rec.Code, rec.Body)
	}
	if rec := proxyCall(t, s, "azure", "box", body, map[string]string{llmrec.HeaderPurpose: "review"}); rec.Code != http.StatusOK {
		t.Fatalf("a tagged call with the switch on: %d", rec.Code)
	}
	if st, _ := s.LLMRec.Stats(); st.Untagged != 2 {
		t.Fatalf("a refused call is not counted: %+v", st)
	}
}
