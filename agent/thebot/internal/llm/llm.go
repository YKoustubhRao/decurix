// Package llm defines "the brain" as a pluggable interface. Ollama is the
// only concrete implementation for now (local-first, zero-config
// default); OpenAI/Anthropic/etc. plug in later as bring-your-own-key,
// opt-in providers without anything above this interface (atomizer,
// scheduler, skills) needing to change - they only ever see Provider.
package llm

import "context"

type Message struct {
	Role    string // "system" | "user" | "assistant"
	Content string
}

type Result struct {
	Text           string
	PromptTokens   int
	ResponseTokens int
	DurationMs     int64
}

type Options struct {
	TimeoutMs int
	// Temperature is forwarded to the provider when non-zero. Left unset
	// (0), each provider uses its own default. Added after a live bug: the
	// atomizer's task-vs-question judgment call used to run at whatever
	// the provider's default happened to be (Ollama's is ~0.8, meant for
	// varied prose), and the SAME planning prompt, sent twice, came back
	// "TASK: yes" once and "TASK: no" the next time - a structured
	// yes/no classification has no business being that random. Deterministic
	// classification calls should pass a low Temperature explicitly.
	Temperature float64
}

type Provider interface {
	Name() string
	Chat(ctx context.Context, model string, messages []Message, opts Options) (Result, error)
	IsReachable(ctx context.Context) bool
}
