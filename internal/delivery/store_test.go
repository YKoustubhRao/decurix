// Tests for the delivery records: sends a crash cut short are found again, an unconfirmed send is taken once, and a
// desktop's message is applied once however many times it is resent.
package delivery

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestLeftoversAreSendsBeforeThisProcess(t *testing.T) {
	store := newStore(t)
	base := time.Now()
	store.now = func() time.Time { return base }
	cut, err := store.Begin("d1", "t1", []byte(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	finished, _ := store.Begin("d1", "t2", nil)
	if err := store.Done(finished); err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return base.Add(time.Minute) }
	if _, err := store.Begin("d1", "t3", nil); err != nil { // this process's own, still in flight
		t.Fatal(err)
	}

	left, err := store.Leftovers(base.Add(30 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].ID != cut || left[0].TaskID != "t1" || string(left[0].Payload) != `{"a":1}` {
		t.Fatalf("leftovers = %+v, want only the cut-short send of t1", left)
	}
}

func TestUnconfirmedIsTakenOnceAndOnlyForItsDesktop(t *testing.T) {
	store := newStore(t)
	id, _ := store.Begin("d1", "t1", nil)
	if err := store.MarkUnconfirmed(id); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := store.TakeUnconfirmed("d2", "t1"); ok {
		t.Fatal("another desktop's outcome took the send")
	}
	if left, _ := store.Leftovers(time.Now().Add(time.Hour)); len(left) != 0 {
		t.Fatalf("an unconfirmed send is still a leftover: %+v", left)
	}
	if _, ok, err := store.TakeUnconfirmed("d1", "t1"); !ok || err != nil {
		t.Fatalf("take = %v, %v; want the send", ok, err)
	}
	if _, ok, _ := store.TakeUnconfirmed("d1", "t1"); ok {
		t.Fatal("the send was taken twice")
	}
	if err := store.MarkUnconfirmed(id); err != ErrNotFound {
		t.Fatalf("marking a removed send = %v, want ErrNotFound", err)
	}
}

func TestApplyOncePerEvent(t *testing.T) {
	store := newStore(t)
	var applied int32
	apply := func() bool { atomic.AddInt32(&applied, 1); return true }

	if dup, err := store.Apply("d1", "e1", apply); dup || err != nil {
		t.Fatalf("first = %v, %v", dup, err)
	}
	if dup, _ := store.Apply("d1", "e1", apply); !dup {
		t.Fatal("a resent message was applied again")
	}
	if dup, _ := store.Apply("d2", "e1", apply); dup {
		t.Fatal("another desktop's message with the same id was taken for a duplicate")
	}
	if dup, _ := store.Apply("d1", "", apply); dup {
		t.Fatal("a message with no id (an older desktop) must always be applied")
	}
	if got := atomic.LoadInt32(&applied); got != 3 {
		t.Fatalf("applied %d times, want 3", got)
	}
}

func TestApplyTwoCopiesTogetherAppliesOnce(t *testing.T) {
	store := newStore(t)
	var applied int32
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = store.Apply("d1", "e1", func() bool {
				atomic.AddInt32(&applied, 1)
				<-release
				return true
			})
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if got := atomic.LoadInt32(&applied); got != 1 {
		t.Fatalf("applied %d times, want once", got)
	}
}

func TestPruneForgetsOldOnly(t *testing.T) {
	store := newStore(t)
	base := time.Now()
	store.now = func() time.Time { return base }
	_, _ = store.Apply("d1", "old", func() bool { return true })
	id, _ := store.Begin("d1", "t1", nil)
	_ = store.MarkUnconfirmed(id)

	store.now = func() time.Time { return base.Add(eventKeep + time.Hour) }
	_, _ = store.Apply("d1", "new", func() bool { return true })
	if err := store.Prune(); err != nil {
		t.Fatal(err)
	}
	if dup, _ := store.Apply("d1", "new", func() bool { return true }); !dup {
		t.Fatal("a recent event was forgotten")
	}
	if dup, _ := store.Apply("d1", "old", func() bool { return true }); dup {
		t.Fatal("an event past its keep was remembered")
	}
	if _, ok, _ := store.TakeUnconfirmed("d1", "t1"); !ok {
		t.Fatal("an unconfirmed send was pruned before its keep")
	}
}

func TestARefusedMessageIsNotRemembered(t *testing.T) {
	store := newStore(t)
	if dup, _ := store.Apply("d1", "e1", func() bool { return false }); dup {
		t.Fatal("first copy read as a duplicate")
	}
	ran := false
	if dup, _ := store.Apply("d1", "e1", func() bool { ran = true; return true }); dup || !ran {
		t.Fatal("a message refused once was taken for applied")
	}
}
