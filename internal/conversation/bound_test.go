package conversation

import (
	"fmt"
	"testing"
)

// Reading a desktop the store knows nothing of creates nothing: any client may name any id (the gated listener
// included), and each such name used to add a thread for good (resource plan B4).
func TestReadingAnUnknownDesktopCreatesNothing(t *testing.T) {
	s := newStore(t)
	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("nobody-%d", i)
		if snap := s.Snapshot(id); len(snap.Entries) != 0 || snap.Running {
			t.Fatalf("an unknown desktop has a chat: %+v", snap)
		}
		_, stop := s.Subscribe(id)
		stop()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.threads) != 0 || len(s.waiting) != 0 {
		t.Fatalf("reads made %d threads and %d waiting lists", len(s.threads), len(s.waiting))
	}
}

// A watcher of a desktop with no chat yet is told when its first entry comes.
func TestAWatcherOfANewChatIsToldOfItsFirstEntry(t *testing.T) {
	s := newStore(t)
	updates, stop := s.Subscribe("fresh")
	defer stop()
	s.AppendUserRequest("fresh", "t1", "hello")
	select {
	case <-updates:
	default:
		t.Fatal("the watcher was not told of the first entry")
	}
	if got := s.Snapshot("fresh"); len(got.Entries) != 1 {
		t.Fatalf("snapshot %+v", got)
	}
}

// The threads kept in memory are bounded; one let go is read back whole from the database, and a watched one is never
// let go.
func TestTheThreadsInMemoryAreBounded(t *testing.T) {
	s := newStore(t)
	updates, stop := s.Subscribe("watched")
	defer stop()
	s.AppendUserRequest("watched", "tw", "keep me")
	// Its turn ended: only its watcher keeps it (review RES-CP-1: a running turn hid that the watcher did not).
	s.EndTurn("watched", "done")
	for i := 0; i < maxThreads+40; i++ {
		s.AppendUserRequest(fmt.Sprintf("box-%d", i), "t", fmt.Sprintf("request %d", i))
		s.EndTurn(fmt.Sprintf("box-%d", i), "done")
	}
	s.mu.Lock()
	n := len(s.threads)
	_, watchedKept := s.threads["watched"]
	s.mu.Unlock()
	if n > maxThreads {
		t.Fatalf("%d threads in memory, bound %d", n, maxThreads)
	}
	if !watchedKept {
		t.Fatal("a watched thread was let go")
	}
	if got := s.Snapshot("box-0"); len(got.Entries) == 0 || got.Entries[0].Content != "request 0" {
		t.Fatalf("a thread let go did not come back from the database: %+v", got)
	}
	select {
	case <-updates:
	default:
	}
}

// A watcher that waited for a chat and joined its thread leaves it when it stops: no subscriber is left behind to pin
// the thread in memory (review RES-CP-1).
func TestAWaitingWatcherThatStopsLeavesItsThread(t *testing.T) {
	s := newStore(t)
	_, stop := s.Subscribe("later")
	s.AppendUserRequest("later", "t1", "hello")
	stop()
	s.mu.Lock()
	defer s.mu.Unlock()
	if th := s.threads["later"]; th == nil || len(th.subscribers) != 0 {
		t.Fatalf("thread %v kept a subscriber after it stopped", th)
	}
	if len(s.waiting) != 0 {
		t.Fatalf("waiting lists left: %v", s.waiting)
	}
}

// A desktop with no chat reads with one version every time (a polling page does not re-render it), and clearing it
// writes nothing.
func TestNoChatIsStableAndClearingItWritesNothing(t *testing.T) {
	s := newStore(t)
	if a, b := s.Snapshot("ghost"), s.Snapshot("ghost"); a.Version != b.Version {
		t.Fatalf("versions %d, %d for no chat", a.Version, b.Version)
	}
	s.Clear("ghost")
	var rows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM chat_threads WHERE desktop_id = 'ghost'`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("clearing no chat wrote %d rows (%v)", rows, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.threads) != 0 {
		t.Fatal("clearing no chat kept a thread")
	}
}
