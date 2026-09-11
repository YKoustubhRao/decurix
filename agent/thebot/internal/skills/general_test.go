package skills

import (
	"context"
	"strings"
	"testing"

	"thebot/internal/llm"
)

type stubChatProvider struct {
	response    string
	err         error
	gotModel    string
	gotMessages []llm.Message
}

func (s *stubChatProvider) Name() string { return "stub" }
func (s *stubChatProvider) Chat(ctx context.Context, model string, messages []llm.Message, opts llm.Options) (llm.Result, error) {
	s.gotModel = model
	s.gotMessages = messages
	if s.err != nil {
		return llm.Result{}, s.err
	}
	return llm.Result{Text: s.response}, nil
}
func (s *stubChatProvider) IsReachable(ctx context.Context) bool { return s.err == nil }

func TestGeneralMatches(t *testing.T) {
	g := NewGeneral()
	if !g.Matches(Subtask{Step: "hello", Type: "general"}) {
		t.Error("Matches() = false for Type=general, want true")
	}
	if g.Matches(Subtask{Step: "hello", Type: "terminal"}) {
		t.Error("Matches() = true for Type=terminal, want false")
	}
}

func TestGeneralRunReturnsRealLLMResponse(t *testing.T) {
	stub := &stubChatProvider{response: "Hi! I'm TheBot."}
	rc := RunContext{Ctx: context.Background(), Provider: stub, Model: "test-model", OnLog: func(string) {}}

	result, err := NewGeneral().Run(rc, Subtask{Step: "Hey. What is your name?", Type: "general"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !result.Verified {
		t.Errorf("Verified = false, want true. Output: %s", result.Output)
	}
	if result.Output != "Hi! I'm TheBot." {
		t.Errorf("Output = %q, want %q", result.Output, "Hi! I'm TheBot.")
	}
	if stub.gotModel != "test-model" {
		t.Errorf("model passed to Chat() = %q, want test-model", stub.gotModel)
	}
}

func TestGeneralRunHandlesLLMErrorGracefully(t *testing.T) {
	stub := &stubChatProvider{err: errStub{}}
	rc := RunContext{Ctx: context.Background(), Provider: stub, Model: "test-model", OnLog: func(string) {}}

	result, err := NewGeneral().Run(rc, Subtask{Step: "hello", Type: "general"})
	if err != nil {
		t.Fatalf("Run() returned a hard error instead of an honest failed result: %v", err)
	}
	if result.Verified {
		t.Error("Verified = true despite the LLM call failing, want false")
	}
}

type errStub struct{}

func (errStub) Error() string { return "simulated LLM failure" }

// TestGeneralRunThreadsSessionHistory is the direct regression test for
// the real bug found live: a second message in the same session had zero
// awareness of the first one. This asserts the actual messages sent to
// the LLM include the prior turns, in order, ending with the new one -
// not just that SOME response comes back.
// The live bug: "how much memory is your RAG using, how many documents
// and chunks do you have indexed" got "I don't have the ability to check"
// even though that exact data is real and available - it just never
// reached the model. When the scheduler supplies it, it must reach the
// model as fact.
func TestGeneralSendsSelfStatsToTheModel(t *testing.T) {
	stub := &stubChatProvider{response: "You have 12 files and 340 chunks indexed."}
	rc := RunContext{
		Ctx: context.Background(), Provider: stub, Model: "test-model", OnLog: func(string) {},
		SelfStats: "Your real current state, queried just now - use these exact numbers if asked about yourself:\n- Data RAG: 12 file(s) indexed, 340 chunk(s) total.\n",
	}

	if _, err := NewGeneral().Run(rc, Subtask{Step: "how much do you have indexed?", Type: "general"}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	var joined strings.Builder
	for _, m := range stub.gotMessages {
		joined.WriteString(m.Content + "\n")
	}
	if !strings.Contains(joined.String(), "340 chunk(s)") {
		t.Errorf("self-stats never reached the model.\nmessages:\n%s", joined.String())
	}
}

// No self-stats supplied (scheduler-less test, or Scheduler had nothing
// configured) must not inject an empty/broken system message.
func TestGeneralWithoutSelfStatsInjectsNothingExtra(t *testing.T) {
	stub := &stubChatProvider{response: "hi"}
	rc := RunContext{Ctx: context.Background(), Provider: stub, Model: "test-model", OnLog: func(string) {}}

	if _, err := NewGeneral().Run(rc, Subtask{Step: "hello", Type: "general"}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	for _, m := range stub.gotMessages {
		if m.Content == "" {
			t.Error("an empty system message was injected")
		}
	}
}

// The user-facing identity requirement: the agent's name is TheBot, and
// the underlying model is only its "brain" - it must never present ITSELF
// as the model.
func TestGeneralSystemPreambleAssertsIdentity(t *testing.T) {
	if !strings.Contains(systemPreamble, "You are TheBot") {
		t.Error("systemPreamble does not assert the agent's own identity as TheBot")
	}
	if !strings.Contains(strings.ToLower(systemPreamble), "brain") {
		t.Error("systemPreamble does not clarify that the underlying model is only the \"brain\", not the agent's identity")
	}
}

func TestGeneralRunThreadsSessionHistory(t *testing.T) {
	stub := &stubChatProvider{response: "your name is Alex"}
	history := []llm.Message{
		{Role: "user", Content: "my name is Alex"},
		{Role: "assistant", Content: "nice to meet you, Alex"},
	}
	rc := RunContext{
		Ctx: context.Background(), Provider: stub, Model: "test-model", OnLog: func(string) {},
		SessionHistory: history,
	}

	_, err := NewGeneral().Run(rc, Subtask{Step: "what is my name", Type: "general"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	got := stub.gotMessages
	if len(got) != 4 {
		t.Fatalf("Chat() received %d messages, want 4 (system + 2 history + new user turn): %+v", len(got), got)
	}
	if got[0].Role != "system" {
		t.Errorf("messages[0].Role = %q, want system", got[0].Role)
	}
	if got[1] != history[0] || got[2] != history[1] {
		t.Errorf("history was not threaded in order: got %+v, %+v; want %+v, %+v", got[1], got[2], history[0], history[1])
	}
	if got[3].Role != "user" || got[3].Content != "what is my name" {
		t.Errorf("messages[3] = %+v, want the new user turn", got[3])
	}
}

func TestGeneralRunWithNoSessionHistoryStillWorks(t *testing.T) {
	stub := &stubChatProvider{response: "hi"}
	rc := RunContext{Ctx: context.Background(), Provider: stub, Model: "test-model", OnLog: func(string) {}}

	_, err := NewGeneral().Run(rc, Subtask{Step: "hello", Type: "general"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(stub.gotMessages) != 2 {
		t.Errorf("Chat() received %d messages, want 2 (system + new user turn) when there's no session history", len(stub.gotMessages))
	}
}
