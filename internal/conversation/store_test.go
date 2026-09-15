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

	s.AppendUserRequest("desktop-1", "", "tidy up /work")

	thread := s.Snapshot("desktop-1")
	if !thread.Running {
		t.Fatal("expected a user request to open a running turn")
	}

	s.AppendStep("desktop-1", "Running: ls -la /work")
	s.AppendStep("desktop-1", "Running: rm -rf /work/tmp")

	if steps := s.Snapshot("desktop-1").Steps; len(steps) != 2 {
		t.Fatalf("expected 2 live steps, got %d", len(steps))
	}

	s.AppendAgentReply("desktop-1", "", KindEntry, "Cleaned up /work.")

	thread = s.Snapshot("desktop-1")

	if thread.Running {
		t.Fatal("expected the agent's reply to close the turn")
	}

	if len(thread.Steps) != 0 {
		t.Fatalf("expected live steps to be cleared, got %d", len(thread.Steps))
	}

	if len(thread.Entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(thread.Entries))
	}

	// The steps that produced an answer travel with it, so a finished turn
	// can still be expanded to show the work behind it.
	if got := len(thread.Entries[1].Steps); got != 2 {
		t.Fatalf("expected the agent's reply to carry its 2 steps, got %d", got)
	}
}

func TestStepWithoutUserRequestOpensTurn(t *testing.T) {
	s := newStore(t)

	s.AppendStep("desktop-1", "Running: whoami")

	if !s.Snapshot("desktop-1").Running {
		t.Fatal("expected an unsolicited step to open a turn rather than be dropped")
	}
}

func TestLateStepJoinsTheTurnItTrails(t *testing.T) {
	s := newStore(t)

	s.AppendUserRequest("desktop-1", "", "write a poem to koustubh.txt")
	s.AppendStep("desktop-1", "skill:file-write -> planning the file to write...")
	s.AppendAgentReply("desktop-1", "", KindEntry, "Wrote /home/headless/work/koustubh.txt")

	// The desktop's step queue delivers its last line just after the reply.
	s.AppendStep("desktop-1", "skill:file-write -> wrote koustubh.txt - verified on disk")

	thread := s.Snapshot("desktop-1")

	if thread.Running {
		t.Fatal("expected a step trailing a reply not to reopen the finished turn")
	}

	if len(thread.Steps) != 0 {
		t.Fatalf("expected no live steps, got %d", len(thread.Steps))
	}

	if got := len(thread.Entries[1].Steps); got != 2 {
		t.Fatalf("expected the late step on the agent's reply it trails, got %d steps", got)
	}
}

func TestUnknownKindFallsBackToEntry(t *testing.T) {
	s := newStore(t)

	entry := s.AppendAgentReply("desktop-1", "", "not-a-kind", "hello")

	if entry.Kind != KindEntry {
		t.Fatalf("expected an unknown kind to fall back to %q, got %q", KindEntry, entry.Kind)
	}
}

func TestThreadsAreIsolatedPerDesktop(t *testing.T) {
	s := newStore(t)

	s.AppendUserRequest("desktop-1", "", "one")
	s.AppendUserRequest("desktop-2", "", "two")

	if got := len(s.Snapshot("desktop-1").Entries); got != 1 {
		t.Fatalf("expected desktop-1 to hold only its own entry, got %d", got)
	}

	if got := s.Snapshot("desktop-2").Entries[0].Content; got != "two" {
		t.Fatalf("expected desktop-2's own entry, got %q", got)
	}
}

func TestVersionAdvancesAndSubscribersWake(t *testing.T) {
	s := newStore(t)

	updates, unsubscribe := s.Subscribe("desktop-1")
	defer unsubscribe()

	before := s.Snapshot("desktop-1").Version

	s.AppendUserRequest("desktop-1", "", "hello")

	if after := s.Snapshot("desktop-1").Version; after <= before {
		t.Fatalf("expected version to advance past %d, got %d", before, after)
	}

	select {
	case <-updates:
	default:
		t.Fatal("expected a subscriber to be woken by a new entry")
	}
}

func TestClearDiscardsTranscript(t *testing.T) {
	s := newStore(t)

	s.AppendUserRequest("desktop-1", "", "hello")
	s.AppendStep("desktop-1", "Running: whoami")
	s.Clear("desktop-1")

	thread := s.Snapshot("desktop-1")

	if len(thread.Entries) != 0 || len(thread.Steps) != 0 || thread.Running {
		t.Fatalf("expected a cleared thread, got %+v", thread)
	}
}

func TestEntriesAreCapped(t *testing.T) {
	s := newStore(t)

	for i := 0; i < maxEntries+20; i++ {
		s.AppendUserRequest("desktop-1", "", "spam")
	}

	if got := len(s.Snapshot("desktop-1").Entries); got != maxEntries {
		t.Fatalf("expected entries to be capped at %d, got %d", maxEntries, got)
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

	s.AppendUserRequest("desktop-1", "task-1", "what is the kernel version?")
	s.AppendStep("desktop-1", "$ uname -r")
	s.AppendActionStep("desktop-1", "click button Save", Action{Kind: "click", Outcome: "done", Before: "/b", After: "/a"})
	s.AppendAgentReply("desktop-1", "task-1", KindEntry, "6.6.87.2")
	s.SetVerdict("desktop-1", "task-1", "correct")
	s.AppendUserRequest("desktop-1", "task-2", "tidy up /work")
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

	if len(after.Entries) != 3 || !after.Running || len(after.Steps) != 1 {
		t.Fatalf("after reopening: %d entries, running=%v, %d live steps", len(after.Entries), after.Running, len(after.Steps))
	}

	answer := after.Entries[1]

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

func TestVerdictMarksOnlyThatTasksAgentReplies(t *testing.T) {
	s := newStore(t)

	s.AppendUserRequest("desktop-1", "task-1", "df -h")
	s.AppendAgentReply("desktop-1", "task-1", KindEntry, "1.6T available")
	s.AppendUserRequest("desktop-1", "task-2", "uname -r")
	s.AppendAgentReply("desktop-1", "task-2", KindEntry, "6.6")

	s.SetVerdict("desktop-1", "task-1", "wrong")

	entries := s.Snapshot("desktop-1").Entries

	if entries[1].Verdict != "wrong" || entries[0].Verdict != "" || entries[3].Verdict != "" {
		t.Fatalf("verdicts: %q %q %q %q", entries[0].Verdict, entries[1].Verdict, entries[2].Verdict, entries[3].Verdict)
	}
}
