package querynotes

import (
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

func newTestStore(t *testing.T) *Store {
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

func note(id, desktop, kind string) QueryNote {
	return QueryNote{ID: id, DesktopID: desktop, VMName: desktop, Filename: id + ".txt", Content: "c-" + id, Kind: kind, CreatedAt: time.Now()}
}

func TestOldCompletedReportsAreHiddenWithoutDeletion(t *testing.T) {
	s := newTestStore(t)
	for _, kind := range []string{"done", "final", "message", "query", "approval", "error"} {
		s.Add(note(kind, "box", kind))
	}
	if got := len(s.List()); got != 3 {
		t.Fatalf("open actionable notes = %d, want 3", got)
	}
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM query_notes").Scan(&count); err != nil || count != 6 {
		t.Fatalf("stored notes = %d, err = %v; historical reports must remain", count, err)
	}
}

// A database written before notes could be resolved (only the first two
// schema steps applied) keeps every note, all of them still open.
func TestNotesFromBeforeResolvingStayOpen(t *testing.T) {
	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := sqlitedb.Migrate(db, "helpnotes", schema[:2]); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Exec(
		`INSERT INTO query_notes (id, desktop_id, vm_name, filename, content, created_ns) VALUES ('old', 'box', 'box', 'o.txt', 'old note', ?)`,
		sqlitedb.ToNS(time.Now()),
	); err != nil {
		t.Fatal(err)
	}

	s, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}

	open := s.List()
	if len(open) != 1 || open[0].ID != "old" || open[0].Kind != "" || open[0].ResolvedAt != nil {
		t.Fatalf("expected the old note open and untyped, got %+v", open)
	}

	// An untyped note is never auto-resolved by a reply (we cannot tell what it was)...
	s.ResolveAnswered("box")
	if len(s.List()) != 1 {
		t.Fatal("a note with no kind must not be resolved by a reply")
	}

	// ...but can still be resolved by hand.
	s.Close("old")
	if len(s.List()) != 0 || len(s.ListResolved()) != 1 {
		t.Fatalf("expected it in the history after Close, open=%d resolved=%d", len(s.List()), len(s.ListResolved()))
	}
}

func TestResolveAnsweredOnlyTouchesWaitingNotesOfThatDesktop(t *testing.T) {
	s := newTestStore(t)

	s.Add(note("q", "a", KindQuery))
	s.Add(note("ap", "a", KindApproval))
	s.Add(note("done", "a", "done"))
	s.Add(note("err", "a", "error"))
	s.Add(note("other", "b", KindQuery))

	s.ResolveAnswered("a")

	open := map[string]bool{}
	for _, n := range s.List() {
		open[n.ID] = true
	}

	if open["q"] || open["ap"] {
		t.Fatalf("the waiting notes should be resolved, open: %v", open)
	}

	if open["done"] || !open["err"] || !open["other"] {
		t.Fatalf("only errors and other desktops' questions should stay visible, open: %v", open)
	}

	for _, n := range s.ListResolved() {
		if n.Resolution != ResolutionAnswered || n.ResolvedAt == nil {
			t.Fatalf("expected answered with a time, got %+v", n)
		}
	}
}

func TestResolvingTwiceKeepsTheFirstResolution(t *testing.T) {
	s := newTestStore(t)
	s.Add(note("q", "a", KindQuery))

	s.ResolveAnswered("a")
	first := s.ListResolved()[0]

	time.Sleep(2 * time.Millisecond)
	s.Close("q")

	again := s.ListResolved()
	if len(again) != 1 || again[0].Resolution != ResolutionAnswered || !again[0].ResolvedAt.Equal(*first.ResolvedAt) {
		t.Fatalf("a second resolve must not rewrite the first: %+v vs %+v", first, again)
	}
}

func TestResolvedHistoryIsNewestFirst(t *testing.T) {
	s := newTestStore(t)
	s.Add(note("one", "a", "done"))
	s.Add(note("two", "a", "done"))

	s.Close("one")
	time.Sleep(2 * time.Millisecond)
	s.Close("two")

	got := s.ListResolved()
	if len(got) != 2 || got[0].ID != "two" || got[1].ID != "one" {
		t.Fatalf("expected most recently resolved first, got %+v", got)
	}
}
