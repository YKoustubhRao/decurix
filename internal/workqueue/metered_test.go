package workqueue

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// meteringSubmitter sends like fakeSubmitter, and each send costs its worker spend at once - as a metered proxy call
// in flight does.
type meteringSubmitter struct {
	*fakeSubmitter
	spend *fakeSpend
	now   func() time.Time
}

func (m *meteringSubmitter) Submit(ctx context.Context, worker, request, dir string, files []File) (string, error) {
	m.spend.entries = append(m.spend.entries, spendEntry{worker, m.now(), Spend{Tokens: 600}})
	return m.fakeSubmitter.Submit(ctx, worker, request, dir, files)
}

// Two items in one tick do not both pass on one reading of a worker's spend: the second sees what the first used
// (C-08).
func TestASecondItemSeesTheSpendOfTheFirst(t *testing.T) {
	e := newEnv(t, Worker{ID: "w1"})
	if _, err := e.store.SetCapacity(Capacity{Worker: "w1", MaxConcurrent: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.SetLimit(Limit{Worker: "w1", Period: PeriodDay, MaxTokens: 1000}); err != nil {
		t.Fatal(err)
	}
	e.sub.busy = nil // capacity, not busyness, decides here
	e.d.Submitter = &meteringSubmitter{fakeSubmitter: e.sub, spend: e.spend, now: e.clock.now}

	first := e.add(t, NewItem{})
	second := e.add(t, NewItem{})
	third := e.add(t, NewItem{})
	e.tick(t)

	sent := 0
	for _, it := range []Item{first, second, third} {
		if e.get(t, it.ID).Status == StatusRunning {
			sent++
		}
	}
	if sent != 2 {
		t.Fatalf("%d items sent on a 1000-token limit at 600 tokens each, want 2 (the third sees 1200 used)", sent)
	}
}

// The proxy's own count raises a worker's spend even when its desktop has reported nothing yet.
func TestMeteredTokensCountWhenTheDesktopSaidNothing(t *testing.T) {
	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := tasks.NewStore(db); err != nil {
		t.Fatal(err)
	}
	ts := TaskSpend{DB: db, Metered: func(string, time.Time) (int64, error) { return 5000, nil }}
	got, err := ts.Spend("w1", time.Now().Add(-time.Hour))
	if err != nil || got.Tokens != 5000 {
		t.Fatalf("spend = %+v, %v", got, err)
	}
}

// A done item's files go after a week; an item that may still be retried keeps its own (C-28).
func TestDoneItemsLoseTheirFilesAfterAWeek(t *testing.T) {
	e := newEnv(t, Worker{ID: "w1"})
	done := e.add(t, NewItem{Files: []File{{Name: "a.txt", Data: []byte("x")}}})
	failed := e.add(t, NewItem{Files: []File{{Name: "b.txt", Data: []byte("y")}}})
	e.store.db.Exec(`UPDATE workqueue_items SET status = ?, updated_ns = 1 WHERE id = ?`, StatusDone, done.ID)
	e.store.db.Exec(`UPDATE workqueue_items SET status = ?, updated_ns = 1 WHERE id = ?`, StatusFailed, failed.ID)
	if n, err := e.store.PruneFiles(time.Now()); err != nil || n != 1 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	if fs, _ := e.store.Files(failed.ID); len(fs) != 1 {
		t.Fatal("a retriable item lost its files")
	}
}

// The limits API says what each limit is checked against: a dollar limit never looks metered (RW1-F-02).
func TestTheLimitsAPISaysTheDollarLimitIsSelfReported(t *testing.T) {
	if !strings.Contains(LimitEnforcement, "Token limits are enforced") || !strings.Contains(LimitEnforcement, "reports about itself") {
		t.Fatalf("enforcement note: %q", LimitEnforcement)
	}
}
