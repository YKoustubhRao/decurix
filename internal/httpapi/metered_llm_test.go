package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/llm"
	"github.com/YKoustubhRao/decurix/internal/tasks"
	"github.com/YKoustubhRao/decurix/internal/workqueue"
)

// A hosted call is metered against the calling desktop, and a desktop at its hard limit gets no more calls (C-08).
func TestTheModelProxyMetersAndEnforcesTheLimit(t *testing.T) {
	db := testDB(t)
	hub, err := llm.NewHub(db, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	hub.ClaudeBaseURL = fakeClaudeAPI(t).URL
	hub.HTTP = http.DefaultClient
	if err := hub.Save(llm.ProviderClaude, llm.Settings{Key: "sk-ant-good", Model: "claude-sonnet-5-5"}); err != nil {
		t.Fatal(err)
	}
	queue, err := workqueue.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	records, err := tasks.NewStore(db) // what the desktops themselves report, beside the meter
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{LLM: hub, Queue: queue, Tasks: records}
	s.QueueDispatcher = &workqueue.Dispatcher{Store: queue, Spend: workqueue.TaskSpend{DB: db, Metered: hub.Metered}}

	call := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/llm/claude/v1/chat/completions",
			strings.NewReader(`{"model":"default","messages":[{"role":"user","content":"hi"}]}`))
		req = req.WithContext(context.WithValue(req.Context(), callerKey{}, "box"))
		rec := httptest.NewRecorder()
		s.llmProxyHandler(rec, req)
		return rec
	}

	if rec := call(); rec.Code != http.StatusOK {
		t.Fatalf("first call: %d %s", rec.Code, rec.Body)
	}
	if got, _ := hub.Metered("box", time.Now()); got != 6 {
		t.Fatalf("metered %d tokens, want the 6 the provider reported", got)
	}

	if _, err := queue.SetLimit(workqueue.Limit{Worker: "box", Period: workqueue.PeriodDay, MaxTokens: 5}); err != nil {
		t.Fatal(err)
	}
	if rec := call(); rec.Code != http.StatusPaymentRequired || !strings.Contains(rec.Body.String(), "limit") {
		t.Fatalf("over the limit: %d %s", rec.Code, rec.Body)
	}
}
