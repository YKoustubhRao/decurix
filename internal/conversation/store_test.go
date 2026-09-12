package conversation

import "testing"

func TestThreadLifecycle(t *testing.T) {
	s := NewStore()

	s.AppendUserMessage("desktop-1", "tidy up /work")

	thread := s.Snapshot("desktop-1")
	if !thread.Running {
		t.Fatal("expected a user message to open a running turn")
	}

	s.AppendStep("desktop-1", "Running: ls -la /work")
	s.AppendStep("desktop-1", "Running: rm -rf /work/tmp")

	if steps := s.Snapshot("desktop-1").Steps; len(steps) != 2 {
		t.Fatalf("expected 2 live steps, got %d", len(steps))
	}

	s.AppendAgentMessage("desktop-1", KindMessage, "Cleaned up /work.")

	thread = s.Snapshot("desktop-1")

	if thread.Running {
		t.Fatal("expected the agent's reply to close the turn")
	}

	if len(thread.Steps) != 0 {
		t.Fatalf("expected live steps to be cleared, got %d", len(thread.Steps))
	}

	if len(thread.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(thread.Messages))
	}

	// The steps that produced an answer travel with it, so a finished turn
	// can still be expanded to show the work behind it.
	if got := len(thread.Messages[1].Steps); got != 2 {
		t.Fatalf("expected the agent message to carry its 2 steps, got %d", got)
	}
}

func TestStepWithoutUserMessageOpensTurn(t *testing.T) {
	s := NewStore()

	s.AppendStep("desktop-1", "Running: whoami")

	if !s.Snapshot("desktop-1").Running {
		t.Fatal("expected an unsolicited step to open a turn rather than be dropped")
	}
}

func TestUnknownKindFallsBackToMessage(t *testing.T) {
	s := NewStore()

	message := s.AppendAgentMessage("desktop-1", "not-a-kind", "hello")

	if message.Kind != KindMessage {
		t.Fatalf("expected an unknown kind to fall back to %q, got %q", KindMessage, message.Kind)
	}
}

func TestThreadsAreIsolatedPerDesktop(t *testing.T) {
	s := NewStore()

	s.AppendUserMessage("desktop-1", "one")
	s.AppendUserMessage("desktop-2", "two")

	if got := len(s.Snapshot("desktop-1").Messages); got != 1 {
		t.Fatalf("expected desktop-1 to hold only its own message, got %d", got)
	}

	if got := s.Snapshot("desktop-2").Messages[0].Content; got != "two" {
		t.Fatalf("expected desktop-2's own message, got %q", got)
	}
}

func TestVersionAdvancesAndSubscribersWake(t *testing.T) {
	s := NewStore()

	updates, unsubscribe := s.Subscribe("desktop-1")
	defer unsubscribe()

	before := s.Snapshot("desktop-1").Version

	s.AppendUserMessage("desktop-1", "hello")

	if after := s.Snapshot("desktop-1").Version; after <= before {
		t.Fatalf("expected version to advance past %d, got %d", before, after)
	}

	select {
	case <-updates:
	default:
		t.Fatal("expected a subscriber to be woken by a new message")
	}
}

func TestClearDiscardsTranscript(t *testing.T) {
	s := NewStore()

	s.AppendUserMessage("desktop-1", "hello")
	s.AppendStep("desktop-1", "Running: whoami")
	s.Clear("desktop-1")

	thread := s.Snapshot("desktop-1")

	if len(thread.Messages) != 0 || len(thread.Steps) != 0 || thread.Running {
		t.Fatalf("expected a cleared thread, got %+v", thread)
	}
}

func TestMessagesAreCapped(t *testing.T) {
	s := NewStore()

	for i := 0; i < maxMessages+20; i++ {
		s.AppendUserMessage("desktop-1", "spam")
	}

	if got := len(s.Snapshot("desktop-1").Messages); got != maxMessages {
		t.Fatalf("expected messages to be capped at %d, got %d", maxMessages, got)
	}
}
