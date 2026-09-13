package llm

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type slowProvider struct {
	inFlight, peak int32
}

func (s *slowProvider) Name() string                         { return "slow" }
func (s *slowProvider) IsReachable(ctx context.Context) bool { return true }
func (s *slowProvider) Chat(ctx context.Context, model string, messages []Message, opts Options) (Result, error) {
	n := atomic.AddInt32(&s.inFlight, 1)
	for {
		peak := atomic.LoadInt32(&s.peak)
		if n <= peak || atomic.CompareAndSwapInt32(&s.peak, peak, n) {
			break
		}
	}
	time.Sleep(20 * time.Millisecond)
	atomic.AddInt32(&s.inFlight, -1)
	return Result{Text: "ok"}, nil
}

func TestLimitedNeverExceedsItsSlots(t *testing.T) {
	inner := &slowProvider{}
	limited := Limited(inner, 2)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := limited.Chat(context.Background(), "m", nil, Options{}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if inner.peak != 2 {
		t.Errorf("peak concurrency = %d, want exactly 2", inner.peak)
	}
}

// A caller waiting for a slot must be able to give up - a cancelled job
// must not sit in the queue until the model frees up.
func TestLimitedHonoursCancellationWhileWaiting(t *testing.T) {
	blocker := make(chan struct{})
	limited := Limited(providerFunc(func() { <-blocker }), 1)
	go limited.Chat(context.Background(), "m", nil, Options{})
	time.Sleep(10 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := limited.Chat(ctx, "m", nil, Options{}); err == nil {
		t.Error("Chat() waited past cancellation for a slot")
	}
	close(blocker)
}

type providerFunc func()

func (f providerFunc) Name() string                         { return "func" }
func (f providerFunc) IsReachable(ctx context.Context) bool { return true }
func (f providerFunc) Chat(ctx context.Context, model string, messages []Message, opts Options) (Result, error) {
	f()
	return Result{}, nil
}
