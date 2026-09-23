package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func activity(t *testing.T, s *Server, ids string) map[string]Activity {
	t.Helper()

	rec := httptest.NewRecorder()
	s.activityHandler(rec, httptest.NewRequest(http.MethodGet, "/api/desktops/activity?ids="+ids, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	var out map[string]Activity
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// A card says what its desktop's chat says: the request being worked on and
// the latest step, and nothing once the turn has ended.
func TestActivityFollowsTheChat(t *testing.T) {
	s := chatServer(t)

	s.Chat.AppendUserRequest("box", "t1", `google "sanmayee is great"`)
	s.Chat.AppendStep("box", "memory: checking L1 for anything relevant...")
	s.Chat.AppendStep("box", "atomizer: recognised as a \"web\" task")

	got := activity(t, s, "box,idle")
	if a := got["box"]; !a.Running || a.Ticket || a.Request != `google "sanmayee is great"` || a.Step != `atomizer: recognised as a "web" task` || a.StartedAt.IsZero() {
		t.Fatalf("box = %+v", a)
	}
	if a, ok := got["idle"]; !ok || a.Running {
		t.Fatalf("idle = %+v, %v", a, ok)
	}

	s.Chat.AppendAgentReply("box", "t1", "entry", "done")
	if a := activity(t, s, "box")["box"]; a.Running {
		t.Fatalf("finished turn still running: %+v", a)
	}
}

func TestActivityNamesATicket(t *testing.T) {
	s := chatServer(t)

	s.Chat.AppendUserRequest("box", "t1", "/record pay the invoice")
	if a := activity(t, s, "box")["box"]; !a.Ticket || a.Request != "pay the invoice" {
		t.Fatalf("ticket = %+v", a)
	}

	s.Chat.AppendAgentReply("box", "t1", "entry", "ok")
	s.Chat.AppendUserRequest("box", "t2", "/recordings please")
	if a := activity(t, s, "box")["box"]; a.Ticket {
		t.Fatalf("not a ticket: %+v", a)
	}
}

func TestClipRunes(t *testing.T) {
	if got := clipRunes("a\n  b", 10); got != "a b" {
		t.Fatalf("%q", got)
	}
	if got := clipRunes("ééééé", 3); got != "éé…" {
		t.Fatalf("%q", got)
	}
}
