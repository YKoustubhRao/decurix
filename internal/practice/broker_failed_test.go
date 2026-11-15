// Tests that a case's usage tells calls the provider answered with a turn from calls it failed or never got: the eval
// runner reads them to tell a case the provider spoiled from one the worker failed (docs/evals.md, "Bookkeeping").
package practice

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/llm"
)

// scriptedChat answers each call in turn from statuses: 0 is "not reached" (an error), any other the status.
type scriptedChat struct {
	mu       sync.Mutex
	statuses []int
}

func (s *scriptedChat) Configured(string) bool { return true }
func (s *scriptedChat) Chat(ctx context.Context, p string, body []byte) (int, []byte, http.Header, error) {
	r, err := s.ChatResult(ctx, p, body)
	return r.Status, r.Body, r.Retry, err
}
func (s *scriptedChat) ChatResult(context.Context, string, []byte) (llm.ChatResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := 200
	if len(s.statuses) > 0 {
		status, s.statuses = s.statuses[0], s.statuses[1:]
	}
	switch {
	case status == 0:
		return llm.ChatResult{}, errors.New("dial tcp: connection refused")
	case status != 200:
		return llm.ChatResult{Status: status, Deployment: "big-1", Body: []byte(`{"error":{"code":"x","message":"busy"}}`)}, nil
	}
	return llm.ChatResult{Status: 200, Deployment: "big-1",
		Body: []byte(`{"model":"big-1","choices":[],"usage":{"prompt_tokens":50,"completion_tokens":5}}`)}, nil
}

func TestCaseUsageCountsProviderFailuresApart(t *testing.T) {
	b := &Broker{LLM: &scriptedChat{statuses: []int{429, 0, 503, 200}}, Caller: desktopAt("practice-r1")}
	b.Begin("practice-r1", 0)
	b.StartCase("t1")
	for i := 0; i < 4; i++ {
		brokerPost(b, "/api/llm/azure/v1/chat/completions", `{"model":"default","messages":[]}`, nil)
	}
	u := b.CaseUsage()
	// Three answers came back (two of them errors), one call never reached the provider.
	if u.Calls != 3 || u.Failed != 2 || u.Unreached != 1 || u.Input != 50 || u.Output != 5 {
		t.Fatalf("usage: %+v", u)
	}
	// A new case starts from nothing.
	b.EndCase()
	b.StartCase("t2")
	if u := b.CaseUsage(); u.Failed != 0 || u.Unreached != 0 || u.Calls != 0 {
		t.Fatalf("a new case's usage: %+v", u)
	}
}
