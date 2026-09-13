package llm

import (
	"context"
	"fmt"
)

// Limited wraps a Provider so that at most n Chat calls run at once, across
// every caller that holds the wrapper.
//
// The scheduler runs independent plan steps concurrently, and the coding
// harness, L1's judge and the web skill all call the model on their own. A
// local Ollama serves one request per loaded model by default
// (OLLAMA_NUM_PARALLEL=1): extra concurrent requests are not answered
// faster, they queue inside Ollama with their timeouts already running, and
// each one that gets in evicts the prompt prefix the previous caller had
// cached. Queueing here instead keeps the wait out of the per-call timeout
// and lets every call reuse the prefix the one before it just evaluated.
//
// Embeddings are not limited - they go to a different, tiny model and are
// served in milliseconds.
func Limited(inner Provider, n int) Provider {
	if n <= 0 {
		n = 1
	}
	return &limitedProvider{inner: inner, slots: make(chan struct{}, n)}
}

type limitedProvider struct {
	inner Provider
	slots chan struct{}
}

func (l *limitedProvider) Name() string { return l.inner.Name() }

func (l *limitedProvider) IsReachable(ctx context.Context) bool { return l.inner.IsReachable(ctx) }

// Chat waits for a free slot, honouring cancellation while it waits, then
// delegates.
func (l *limitedProvider) Chat(ctx context.Context, model string, messages []Message, opts Options) (Result, error) {
	select {
	case l.slots <- struct{}{}:
	case <-ctx.Done():
		return Result{}, fmt.Errorf("waiting for a free model slot: %w", ctx.Err())
	}
	defer func() { <-l.slots }()
	return l.inner.Chat(ctx, model, messages, opts)
}

// Embed passes straight through when the wrapped provider can embed, so a
// Limited provider can still be handed to anything that wants an Embedder.
func (l *limitedProvider) Embed(ctx context.Context, model, text string) ([]float32, error) {
	e, ok := l.inner.(Embedder)
	if !ok {
		return nil, fmt.Errorf("%s cannot embed", l.inner.Name())
	}
	return e.Embed(ctx, model, text)
}
