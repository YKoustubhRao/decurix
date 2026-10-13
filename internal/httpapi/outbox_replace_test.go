package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/outbox"
	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

func newOutboxServer(t *testing.T) *Server {
	t.Helper()
	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store, err := outbox.New(db)
	if err != nil {
		t.Fatal(err)
	}
	return &Server{Outbox: store}
}

func draftVia(t *testing.T, s *Server, body string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.outboxCreateHandler(rec, httptest.NewRequest(http.MethodPost, "/api/outbox", strings.NewReader(body)))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// A worker sent back to correct its work replaces the drafts it made, instead of adding a second copy of each.
func TestADraftReplacesOnlyAnUnsentDraftOfTheSameTask(t *testing.T) {
	s := newOutboxServer(t)
	_, first := draftVia(t, s, `{"desktop_id":"ops","task_id":"t1","to":["a@x.com"],"subject":"Reminder","body":"v1"}`)
	id := first["id"].(string)

	code, second := draftVia(t, s, `{"desktop_id":"ops","task_id":"t1","to":["a@x.com"],"subject":"Reminder","body":"v2","replaces":"`+id+`"}`)
	if code != http.StatusOK || second["replaced"] != id {
		t.Fatalf("replace = %d %v, want 200 replacing %s", code, second, id)
	}
	old, _ := s.Outbox.Get(id)
	if old.Status != outbox.StatusDiscarded {
		t.Fatalf("the replaced draft is %q, want discarded", old.Status)
	}

	// Another task, or another desktop, cannot withdraw it; nor can anything withdraw a draft twice.
	newID := second["id"].(string)
	for _, body := range []string{
		`{"desktop_id":"ops","task_id":"t2","to":["a@x.com"],"subject":"x","body":"y","replaces":"` + newID + `"}`,
		`{"desktop_id":"other","task_id":"t1","to":["a@x.com"],"subject":"x","body":"y","replaces":"` + newID + `"}`,
		`{"desktop_id":"ops","task_id":"","to":["a@x.com"],"subject":"x","body":"y","replaces":"` + newID + `"}`,
		`{"desktop_id":"ops","task_id":"t1","to":["a@x.com"],"subject":"x","body":"y","replaces":"` + id + `"}`,
	} {
		if code, out := draftVia(t, s, body); code != http.StatusConflict {
			t.Errorf("%s = %d %v, want 409", body, code, out)
		}
	}
	kept, _ := s.Outbox.Get(newID)
	if kept.Status != outbox.StatusDraft {
		t.Fatalf("the replacement is %q, want still a draft", kept.Status)
	}
}
