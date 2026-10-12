package llm

import (
	"context"
	"errors"
	"testing"
)

type countingProvider struct {
	res Result
	err error
}

func (c *countingProvider) Name() string                         { return "counting" }
func (c *countingProvider) IsReachable(ctx context.Context) bool { return true }
func (c *countingProvider) Chat(ctx context.Context, model string, messages []Message, opts Options) (Result, error) {
	return c.res, c.err
}

func TestMeteredAddsEachCallToTheJobsUsage(t *testing.T) {
	inner := &countingProvider{res: Result{PromptTokens: 900, ResponseTokens: 40}}
	// Wrapped the way soul wraps it: metered inside the concurrency limit.
	provider := Limited(Metered(inner), 1)

	ctx, used := WithUsage(context.Background())
	for i := 0; i < 3; i++ {
		if _, err := provider.Chat(ctx, "m", nil, Options{}); err != nil {
			t.Fatal(err)
		}
	}
	// A derived context (a per-call timeout) still counts for the same job.
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	if _, err := provider.Chat(child, "m", nil, Options{}); err != nil {
		t.Fatal(err)
	}

	prompt, response, calls := used.Snapshot()
	if prompt != 3600 || response != 160 || calls != 4 {
		t.Fatalf("usage = %d/%d over %d calls, want 3600/160 over 4", prompt, response, calls)
	}
}

func TestMeteredLeavesOtherJobsAndFailuresAlone(t *testing.T) {
	inner := &countingProvider{res: Result{PromptTokens: 10, ResponseTokens: 1}}
	provider := Metered(inner)

	_, mine := WithUsage(context.Background())
	theirs, other := WithUsage(context.Background())
	if _, err := provider.Chat(theirs, "m", nil, Options{}); err != nil {
		t.Fatal(err)
	}
	// No usage in the context: passed through, nothing to count into.
	if _, err := provider.Chat(context.Background(), "m", nil, Options{}); err != nil {
		t.Fatal(err)
	}
	if p, _, calls := mine.Snapshot(); p != 0 || calls != 0 {
		t.Fatalf("a job was charged for another job's call: %d tokens, %d calls", p, calls)
	}
	if p, _, calls := other.Snapshot(); p != 10 || calls != 1 {
		t.Fatalf("usage = %d over %d calls, want 10 over 1", p, calls)
	}

	// A call that failed and reported nothing is not a call that was paid for.
	failing := Metered(&countingProvider{err: errors.New("boom")})
	ctx, used := WithUsage(context.Background())
	if _, err := failing.Chat(ctx, "m", nil, Options{}); err == nil {
		t.Fatal("expected the error to pass through")
	}
	if _, _, calls := used.Snapshot(); calls != 0 {
		t.Fatalf("a failed call was counted: %d", calls)
	}

	var none *Usage
	if p, r, c := none.Snapshot(); p+r+c != 0 {
		t.Fatal("a nil usage must read as zero")
	}
}
