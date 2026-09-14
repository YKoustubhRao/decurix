package conversation

import (
	"path/filepath"
	"testing"

	"github.com/YKoustubhRao/flowera/internal/sqlitedb"
)

func newStore(t *testing.T) *Store {
	t.Helper()

	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	s, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}

	return s
}

func TestThreadLifecycle(t *testing.T) {
	s := newStore(t)

	s.AppendUserMessage("desktop-1", "", "tidy up /work")

	thread := s.Snapshot("desktop-1")
	if !thread.Running {
		t.Fatal("expected a user message to open a running turn")
	}

	s.AppendStep("desktop-1", "Running: ls -la /work")
	s.AppendStep("desktop-1", "Running: rm -rf /work/tmp")

	if steps := s.Snapshot("desktop-1").Steps; len(steps) != 2 {
		t.Fatalf("expected 2 live steps, got %d", len(steps))
	}

	s.AppendAgentMessage("desktop-1", "", KindMessage, "Cleaned up /work.")

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
	s := newStore(t)

	s.AppendStep("desktop-1", "Running: whoami")

	if !s.Snapshot("desktop-1").Running {
		t.Fatal("expected an unsolicited step to open a turn rather than be dropped")
	}
}

func TestLateStepJoinsTheTurnItTrails(t *testing.T) {
	s := newStore(t)

	s.AppendUserMessage("desktop-1", "", "write a poem to koustubh.txt")
	s.AppendStep("desktop-1", "skill:file-write -> planning the file to write...")
	s.AppendAgentMessage("desktop-1", "", KindMessage, "Wrote /home/headless/work/koustubh.txt")

	// The desktop's step queue delivers its last line just after the reply.
	s.AppendStep("desktop-1", "skill:file-write -> wrote koustubh.txt - verified on disk")

	thread := s.Snapshot("desktop-1")

	if thread.Running {
		t.Fatal("expected a step trailing a reply not to reopen the finished turn")
	}

	if len(thread.Steps) != 0 {
		t.Fatalf("expected no live steps, got %d", len(thread.Steps))
	}

	if got := len(thread.Messages[1].Steps); got != 2 {
		t.Fatalf("expected the late step on the agent message it trails, got %d steps", got)
	}
}

func TestUnknownKindFallsBackToMessage(t *testing.T) {
	s := newStore(t)

	message := s.AppendAgentMessage("desktop-1", "", "not-a-kind", "hello")

	if message.Kind != KindMessage {
		t.Fatalf("expected an unknown kind to fall back to %q, got %q", KindMessage, message.Kind)
	}
}

func TestThreadsAreIsolatedPerDesktop(t *testing.T) {
	s := newStore(t)

	s.AppendUserMessage("desktop-1", "", "one")
	s.AppendUserMessage("desktop-2", "", "two")

	if got := len(s.Snapshot("desktop-1").Messages); got != 1 {
		t.Fatalf("expected desktop-1 to hold only its own message, got %d", got)
	}

	if got := s.Snapshot("desktop-2").Messages[0].Content; got != "two" {
		t.Fatalf("expected desktop-2's own message, got %q", got)
	}
}

func TestVersionAdvancesAndSubscribersWake(t *testing.T) {
	s := newStore(t)

	updates, unsubscribe := s.Subscribe("desktop-1")
	defer unsubscribe()

	before := s.Snapshot("desktop-1").Version

	s.AppendUserMessage("desktop-1", "", "hello")

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
	s := newStore(t)

	s.AppendUserMessage("desktop-1", "", "hello")
	s.AppendStep("desktop-1", "Running: whoami")
	s.Clear("desktop-1")

	thread := s.Snapshot("desktop-1")

	if len(thread.Messages) != 0 || len(thread.Steps) != 0 || thread.Running {
		t.Fatalf("expected a cleared thread, got %+v", thread)
	}
}

func TestMessagesAreCapped(t *testing.T) {
	s := newStore(t)

	for i := 0; i < maxMessages+20; i++ {
		s.AppendUserMessage("desktop-1", "", "spam")
	}

	if got := len(s.Snapshot("desktop-1").Messages); got != maxMessages {
		t.Fatalf("expected messages to be capped at %d, got %d", maxMessages, got)
	}
}

// "A restart loses nothing": a finished turn with its steps, a turn still
// running, and a verdict all come back when the store is reopened on the
// same file.
func TestThreadSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), sqlitedb.FileName)

	db, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}

	s, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}

	s.AppendUserMessage("desktop-1", "task-1", "what is the kernel version?")
	s.AppendStep("desktop-1", "$ uname -r")
	s.AppendActionStep("desktop-1", "click button Save", Action{Kind: "click", Outcome: "done", Before: "/b", After: "/a"})
	s.AppendAgentMessage("desktop-1", "task-1", KindMessage, "6.6.87.2")
	s.SetVerdict("desktop-1", "task-1", "correct")
	s.AppendUserMessage("desktop-1", "task-2", "tidy up /work")
	s.AppendStep("desktop-1", "$ ls /work")

	before := s.Snapshot("desktop-1")
	db.Close()

	db, err = sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	s, err = NewStore(db)
	if err != nil {
		t.Fatal(err)
	}

	after := s.Snapshot("desktop-1")

	if len(after.Messages) != 3 || !after.Running || len(after.Steps) != 1 {
		t.Fatalf("after reopening: %d messages, running=%v, %d live steps", len(after.Messages), after.Running, len(after.Steps))
	}

	answer := after.Messages[1]

	if answer.Content != "6.6.87.2" || answer.TaskID != "task-1" || answer.Verdict != "correct" {
		t.Fatalf("answer after reopening: %+v", answer)
	}

	if len(answer.Steps) != 2 || answer.Steps[1].Action == nil || answer.Steps[1].Action.After != "/a" {
		t.Fatalf("the answer's steps did not come back intact: %+v", answer.Steps)
	}

	if !after.StartedAt.Equal(before.StartedAt) {
		t.Fatalf("started_at %v, want %v", after.StartedAt, before.StartedAt)
	}

	if after.Version <= before.Version {
		t.Fatalf("version %d after a restart must not repeat one a browser already has (%d)", after.Version, before.Version)
	}
}

func TestVerdictMarksOnlyThatTasksAgentMessages(t *testing.T) {
	s := newStore(t)

	s.AppendUserMessage("desktop-1", "task-1", "df -h")
	s.AppendAgentMessage("desktop-1", "task-1", KindMessage, "1.6T available")
	s.AppendUserMessage("desktop-1", "task-2", "uname -r")
	s.AppendAgentMessage("desktop-1", "task-2", KindMessage, "6.6")

	s.SetVerdict("desktop-1", "task-1", "wrong")

	messages := s.Snapshot("desktop-1").Messages

	if messages[1].Verdict != "wrong" || messages[0].Verdict != "" || messages[3].Verdict != "" {
		t.Fatalf("verdicts: %q %q %q %q", messages[0].Verdict, messages[1].Verdict, messages[2].Verdict, messages[3].Verdict)
	}
}
