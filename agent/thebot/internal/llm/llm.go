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
	// Truncated reports that the provider stopped because it ran out of
	// room, not because the answer was finished. The content is real but
	// INCOMPLETE - cut off mid-sentence, mid-function, mid-JSON.
	//
	// Worth a dedicated field because the failure is otherwise invisible:
	// the HTTP call is a clean 200 and the body looks like an answer. The
	// coding harness spent nine attempts "fixing" a Python file that had
	// simply been cut off, because a truncated file and a badly written
	// file fail in exactly the same way - with a syntax error.
	Truncated bool
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
	// NumCtx is the total context window (prompt AND response together) to
	// ask the provider for, where it lets you choose. Unset, the provider's
	// own default applies.
	//
	// This exists because of a real, expensive failure found the first time
	// the coding harness built a project against a live model. Ollama's
	// window was a fixed 8192 for every call, and a code-generation prompt
	// is nothing like a chat turn: it carries the whole architecture, the
	// real content of every file written so far, and the previous attempt's
	// real compiler output. That is easily 5-6k tokens of prompt on its
	// own, leaving a few hundred for the answer - so the model emitted a
	// file cut off mid-function. Ollama does not report that: it returns a
	// clean 200 with a truncated body. The file then failed to compile, the
	// harness regenerated it, the same truncation happened again, and nine
	// attempts in a row were spent on a "stubborn model" that had never
	// been given room to finish a sentence.
	NumCtx int
	// MaxTokens caps the response itself, set alongside NumCtx so a long
	// file has both the room to exist and permission to be long.
	MaxTokens int
}

type Provider interface {
	Name() string
	Chat(ctx context.Context, model string, messages []Message, opts Options) (Result, error)
	IsReachable(ctx context.Context) bool
}
