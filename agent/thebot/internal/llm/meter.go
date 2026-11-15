package llm

import (
	"context"
	"fmt"
	"sync"
)

// Usage is what one job's model calls read and wrote, summed. A host that
// wants a worker to get cheaper with use has to be able to see what each
// task cost: soul used to report none of it, so a hosted brain's whole
// spend - nearly all of a task's tokens - was invisible to the task records.
type Usage struct {
	mu             sync.Mutex
	PromptTokens   int
	ResponseTokens int
	Calls          int
	// CachedTokens is the part of PromptTokens read from the provider's prompt cache.
	CachedTokens int
}

// Cached is the prompt tokens so far that the provider read from its cache.
func (u *Usage) Cached() int {
	if u == nil {
		return 0
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.CachedTokens
}

// Snapshot returns the totals so far.
func (u *Usage) Snapshot() (prompt, response, calls int) {
	if u == nil {
		return 0, 0, 0
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.PromptTokens, u.ResponseTokens, u.Calls
}

func (u *Usage) add(r Result) {
	u.mu.Lock()
	u.PromptTokens += r.PromptTokens
	u.ResponseTokens += r.ResponseTokens
	u.CachedTokens += r.CachedTokens
	u.Calls++
	u.mu.Unlock()
}

type usageKey struct{}

// WithUsage returns a context whose model calls, made through a Metered
// provider, are added to the returned Usage.
func WithUsage(ctx context.Context) (context.Context, *Usage) {
	u := &Usage{}
	return context.WithValue(ctx, usageKey{}, u), u
}

// Metered wraps a Provider so every Chat call's token counts are added to the
// Usage its context carries (WithUsage). A call with no Usage in its context
// is passed through uncounted.
//
// It also reads each answer against the conversation it belongs to (conversationID): two zero-output answers in a row
// end as ErrEmptyAnswer (empty.go), and the real prompt counts teach what a picture costs on the route (imagecost.go).
func Metered(inner Provider) Provider { return &meteredProvider{inner: inner} }

type meteredProvider struct {
	inner Provider
	convs conversations
}

func (m *meteredProvider) Name() string { return m.inner.Name() }

func (m *meteredProvider) IsReachable(ctx context.Context) bool { return m.inner.IsReachable(ctx) }

// Unwrap is the provider Metered wraps (RouteOf).
func (m *meteredProvider) Unwrap() Provider { return m.inner }

// Chat delegates, then counts what the call used - a failed call too, when
// the provider reported counts for it.
func (m *meteredProvider) Chat(ctx context.Context, model string, messages []Message, opts Options) (Result, error) {
	opts = tagPurpose(opts)
	res, err := m.inner.Chat(ctx, model, messages, opts)
	if u, ok := ctx.Value(usageKey{}).(*Usage); ok && u != nil && (err == nil || res.PromptTokens > 0) {
		u.add(res)
	}
	if err != nil {
		return res, err
	}
	route := RouteOf(m.inner, model)
	id := conversationID(route, TagsFrom(ctx).Job, opts.Purpose, messages)
	var now *usageAnchor
	if res.PromptTokens > 0 {
		now = anchorFor(messages, res.PromptTokens) // hashed outside the lock
	}
	m.convs.mu.Lock()
	defer m.convs.mu.Unlock()
	conv := m.convs.get(id)
	if now != nil {
		imageCosts.learn(route, conv.anchor, messages, now)
		conv.anchor = now
	}
	if empty := conv.noteAnswer(route, res); empty != nil {
		return res, empty
	}
	return res, nil
}

// Embed passes straight through, as Limited's does.
func (m *meteredProvider) Embed(ctx context.Context, model, text string) ([]float32, error) {
	e, ok := m.inner.(Embedder)
	if !ok {
		return nil, fmt.Errorf("%s cannot embed", m.inner.Name())
	}
	return e.Embed(ctx, model, text)
}
