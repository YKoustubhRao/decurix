package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// A desktop back from a restart reads which of its own tasks wait on a person (GET /api/agent/waiting), so its agent
// can close each question it no longer holds; it never sees another desktop's, and cannot ask as one (found live
// 2026-11-09: a task read "waiting on you" for good over a question the restarted agent had forgotten).
func TestADesktopReadsItsOwnWaitingTasksOnly(t *testing.T) {
	s := chatServer(t)
	box := tasks.Worker{ID: "box", Name: "box"}
	other := tasks.Worker{ID: "other", Name: "other"}
	asked, _ := s.Tasks.Commit(tasks.NewRoute(), box, "enter the October bills", tasks.Origin{Kind: tasks.OriginPerson})
	_, _ = s.Tasks.RecordOutcome("box", asked.TaskID, tasks.Outcome{Kind: tasks.OutcomeQuery, Reply: "Which folder?"})
	theirs, _ := s.Tasks.Commit(tasks.NewRoute(), other, "something else", tasks.Origin{Kind: tasks.OriginPerson})
	_, _ = s.Tasks.RecordOutcome("other", theirs.TaskID, tasks.Outcome{Kind: tasks.OutcomeQuery, Reply: "Which one?"})

	rec := getAsDesktop(s, s.agentWaitingHandler, "box", "/api/agent/waiting")
	var got struct {
		Tasks []string `json:"tasks"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(got.Tasks) != 1 || got.Tasks[0] != asked.TaskID {
		t.Fatalf("tasks = %v, want only box's own %s", got.Tasks, asked.TaskID)
	}
	if rec := getAsDesktop(s, s.agentWaitingHandler, "box", "/api/agent/waiting?desktop_id=other"); rec.Code != http.StatusForbidden {
		t.Fatalf("a desktop read another's tasks: %d %s", rec.Code, rec.Body)
	}
}
