package llm

// empty_test.go: two zero-output answers in a row end the asking; anything less certain is left to the caller.

import (
	"context"
	"errors"
	"testing"
)

// scriptedProvider answers each call with the next result of its script.
type scriptedProvider struct {
	script []Result
	calls  int
}

func (s *scriptedProvider) Name() string                         { return "scripted" }
func (s *scriptedProvider) IsReachable(ctx context.Context) bool { return true }
func (s *scriptedProvider) Chat(ctx context.Context, model string, messages []Message, opts Options) (Result, error) {
	r := s.script[min(s.calls, len(s.script)-1)]
	s.calls++
	return r, nil
}

func TestTwoZeroOutputAnswersInARowCouldNotBeAsked(t *testing.T) {
	empty := Result{PromptTokens: 30000, ResponseTokens: 0, FinishReason: "stop"}
	answer := Result{Text: "COMMAND: ls", PromptTokens: 30000, ResponseTokens: 5, FinishReason: "stop"}
	conv := []Message{{Role: "system", Content: "frame"}, {Role: "user", Content: "the request"}}
	ask := func(p Provider, ctx context.Context) error {
		_, err := p.Chat(ctx, "m", conv, Options{Purpose: PurposeOperatorTurn})
		return err
	}
	for _, c := range []struct {
		name   string
		script []Result
		failAt int // the call (1-based) that fails as ErrEmptyAnswer, 0 for none
	}{
		{"two empties", []Result{empty, empty}, 2},
		{"an answer between them", []Result{empty, answer, empty, answer}, 0},
		{"different finish reasons", []Result{empty, {PromptTokens: 30000, FinishReason: "length"}}, 0},
		{"output tokens spent (reasoning)", []Result{{PromptTokens: 30000, ResponseTokens: 900, FinishReason: "stop"}, {PromptTokens: 30000, ResponseTokens: 900, FinishReason: "stop"}}, 0},
		{"no usage reported", []Result{{FinishReason: "stop"}, {FinishReason: "stop"}}, 0},
		{"whitespace counted as output", []Result{{Text: " \n", PromptTokens: 9, ResponseTokens: 1}, {Text: "\n", PromptTokens: 9, ResponseTokens: 1}}, 0},
	} {
		inner := &scriptedProvider{script: c.script}
		p := Limited(Metered(inner), 1)
		ctx := WithTags(context.Background(), Tags{Job: "j1"})
		failed := 0
		for i := 1; i <= len(c.script); i++ {
			if err := ask(p, ctx); err != nil {
				if !errors.Is(err, ErrEmptyAnswer) || Transient(err) {
					t.Fatalf("%s: call %d: %v (want ErrEmptyAnswer, not transient)", c.name, i, err)
				}
				if failed == 0 {
					failed = i
				}
			}
		}
		if failed != c.failAt {
			t.Errorf("%s: failed at call %d, want %d", c.name, failed, c.failAt)
		}
	}
}

// The streak is the conversation's: an empty answer in one goal and one in another are not two in a row.
func TestEmptyAnswersInDifferentConversationsAreNotAStreak(t *testing.T) {
	empty := Result{PromptTokens: 100, FinishReason: "stop"}
	p := Metered(&scriptedProvider{script: []Result{empty}})
	goal := func(request string) []Message {
		return []Message{{Role: "system", Content: "frame"}, {Role: "user", Content: request}}
	}
	ctx := context.Background()
	if _, err := p.Chat(ctx, "m", goal("first goal"), Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Chat(ctx, "m", goal("second goal"), Options{}); err != nil {
		t.Fatalf("another conversation's empty answer counted: %v", err)
	}
	if _, err := p.Chat(WithTags(ctx, Tags{Job: "other"}), "m", goal("first goal"), Options{}); err != nil {
		t.Fatalf("another job's empty answer counted: %v", err)
	}
	if _, err := p.Chat(ctx, "m", goal("first goal"), Options{}); !errors.Is(err, ErrEmptyAnswer) {
		t.Fatalf("the first goal's second empty answer: %v", err)
	}
}

func TestRouteOfFindsTheEndpointUnderTheWrappers(t *testing.T) {
	o := NewOllama("http://ollama:11434")
	wrapped := Limited(Metered(o), 1)
	got := RouteOf(wrapped, "qwen")
	if got != (RouteKey{Provider: "ollama", Model: "qwen", Endpoint: "http://ollama:11434"}) {
		t.Fatalf("route %+v", got)
	}
	if RouteOf(Metered(&scriptedProvider{}), "m").Endpoint != "" {
		t.Fatal("a provider that names no endpoint was given one")
	}
}
