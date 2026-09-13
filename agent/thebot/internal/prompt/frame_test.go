package prompt

import (
	"strings"
	"testing"
	"time"
)

// The frame is only worth anything if two calls in the same job render it
// byte for byte the same - that is what Ollama's prefix cache matches on.
func TestFrameIsStableAndOrderedMostStableFirst(t *testing.T) {
	now := time.Date(2026, 9, 12, 23, 15, 0, 0, time.UTC)
	f := Frame{Host: "Desktop name: kilo", Facts: []string{"The user's name is Asha."}, Stats: "- Data RAG: 3 file(s)", Now: now, WithTime: true}
	a, b := f.Message(), f.Message()
	if a != b {
		t.Fatal("the same frame rendered differently twice")
	}
	c := a.Content
	persona, host, facts, stats, clock := strings.Index(c, "You are TheBot"), strings.Index(c, "kilo"), strings.Index(c, "Asha"), strings.Index(c, "Data RAG"), strings.Index(c, "Current date")
	if !(persona == 0 && persona < host && host < facts && facts < stats && stats < clock) {
		t.Errorf("sections out of order (persona %d, host %d, facts %d, stats %d, clock %d)", persona, host, facts, stats, clock)
	}
}

func TestFrameOmitsEmptySections(t *testing.T) {
	c := Frame{}.Message().Content
	if c != Persona {
		t.Errorf("an empty frame should be exactly the persona, got:\n%s", c)
	}
}

func TestIsNeedsTools(t *testing.T) {
	for _, reply := range []string{"NEEDS_TOOLS", "**NEEDS_TOOLS**", " NEEDS_TOOLS.\n"} {
		if !IsNeedsTools(reply) {
			t.Errorf("IsNeedsTools(%q) = false", reply)
		}
	}
	for _, reply := range []string{"I'm TheBot.", "You can check with df -h - no NEEDS_TOOLS needed, here is a long explanation of why"} {
		if IsNeedsTools(reply) {
			t.Errorf("IsNeedsTools(%q) = true", reply)
		}
	}
}

// Without the time of day, two jobs the same day open with the same bytes.
func TestFrameWithoutTimeIsStableAcrossTheDay(t *testing.T) {
	morning := Frame{Host: "h", Now: time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)}.Message()
	evening := Frame{Host: "h", Now: time.Date(2026, 9, 12, 21, 30, 0, 0, time.UTC)}.Message()
	if morning != evening {
		t.Errorf("date-only frames differ within a day:\n%s\n---\n%s", morning.Content, evening.Content)
	}
}

// Only the chat skill may ask for tools; every other call shares the persona.
func TestPersonaDoesNotCarryTheChatSignal(t *testing.T) {
	if strings.Contains(Persona, NeedsToolsMarker) {
		t.Error("the persona teaches NEEDS_TOOLS to every call, including the operator's")
	}
}
