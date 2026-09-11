package session

import (
	"testing"

	"thebot/internal/llm"
)

func TestHistoryEmptyForUnknownSession(t *testing.T) {
	s := NewStore()
	h := s.History("nope")
	if len(h) != 0 {
		t.Errorf("History() for an unknown session = %v, want empty", h)
	}
}

func TestAppendAndHistoryRealOrder(t *testing.T) {
	s := NewStore()
	s.Append("sess1", llm.Message{Role: "user", Content: "my name is Alex"})
	s.Append("sess1", llm.Message{Role: "assistant", Content: "nice to meet you, Alex"})
	s.Append("sess1", llm.Message{Role: "user", Content: "what is my name"})

	h := s.History("sess1")
	if len(h) != 3 {
		t.Fatalf("History() length = %d, want 3", len(h))
	}
	if h[0].Content != "my name is Alex" || h[2].Content != "what is my name" {
		t.Errorf("History() = %+v, wrong order", h)
	}
}

func TestSessionsAreIsolated(t *testing.T) {
	s := NewStore()
	s.Append("a", llm.Message{Role: "user", Content: "secret A"})
	s.Append("b", llm.Message{Role: "user", Content: "secret B"})

	ha := s.History("a")
	hb := s.History("b")
	if len(ha) != 1 || ha[0].Content != "secret A" {
		t.Errorf("session a history = %+v, want just secret A", ha)
	}
	if len(hb) != 1 || hb[0].Content != "secret B" {
		t.Errorf("session b history = %+v, want just secret B", hb)
	}
}

func TestHistoryReturnsACopyNotSharedState(t *testing.T) {
	s := NewStore()
	s.Append("sess1", llm.Message{Role: "user", Content: "first"})

	h := s.History("sess1")
	h[0].Content = "mutated by caller"

	h2 := s.History("sess1")
	if h2[0].Content != "first" {
		t.Errorf("History() leaked internal state - a caller mutating its copy changed the store's own data: %+v", h2)
	}
}

func TestAppendIsNoOpForEmptySessionID(t *testing.T) {
	s := NewStore()
	s.Append("", llm.Message{Role: "user", Content: "no session"})
	if len(s.sessions) != 0 {
		t.Errorf("Append(\"\", ...) created a real bucket, want no-op: %v", s.sessions)
	}
}

func TestHistoryCapsAtMaxMessages(t *testing.T) {
	s := NewStore()
	for i := 0; i < maxHistoryMessages+10; i++ {
		s.Append("sess1", llm.Message{Role: "user", Content: "msg"})
	}
	h := s.History("sess1")
	if len(h) != maxHistoryMessages {
		t.Errorf("History() length = %d, want capped at %d", len(h), maxHistoryMessages)
	}
}
