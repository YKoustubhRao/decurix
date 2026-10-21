// Package llm defines "the brain" as a pluggable interface. Ollama is the
// only concrete implementation for now (local-first, zero-config
// default); OpenAI/Anthropic/etc. plug in later as bring-your-own-key,
// opt-in providers without anything above this interface (atomizer,
// scheduler, skills) needing to change - they only ever see Provider.
package llm

import (
	"context"
	"fmt"
	"strings"
)

type Message struct {
	Role    string // "system" | "user" | "assistant"
	Content string
	// Images are PNGs shown with a user message - only ever set for a brain
	// with Capabilities.Vision. Every provider sends them in its own shape.
	Images [][]byte `json:",omitempty"`
	// ImageLabels says what each picture is ("scan_0030.pdf, page 1"), by
	// index; shown right before its picture where the provider lets text and
	// pictures interleave. Found live (2026-10-14): two scans read in one
	// command reached the model as two unlabelled pictures after the text, it
	// took one invoice for the other - and "verified" the swap by looking again.
	ImageLabels []string `json:",omitempty"`
}

// ImageLabel is the label of picture i of a message, or "".
func (m Message) ImageLabel(i int) string {
	if i < len(m.ImageLabels) {
		return m.ImageLabels[i]
	}
	return ""
}

// LabelledContent is a message's text with its pictures' labels listed in
// order after it - for a provider that cannot put text between pictures.
func (m Message) LabelledContent() string {
	var labels []string
	for i := range m.Images {
		if l := m.ImageLabel(i); l != "" {
			labels = append(labels, fmt.Sprintf("%d) %s", i+1, l))
		}
	}
	if len(labels) == 0 {
		return m.Content
	}
	return m.Content + "\n\n(The pictures, in order: " + strings.Join(labels, "; ") + ")"
}

type Result struct {
	Text           string
	PromptTokens   int
	ResponseTokens int
	// CachedTokens is the part of PromptTokens the provider read from its prompt cache (a repeated prefix, billed
	// at a fraction of the price), when it says so; 0 otherwise.
	CachedTokens int
	DurationMs   int64
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
	// KeepAlive overrides the provider's own default for this call alone.
	// Unset, the provider's default applies (Ollama's is OLLAMA_KEEP_ALIVE,
	// 30m).
	//
	// Exists for the other half of the NumCtx problem above: Ollama treats
	// the same model at two different num_ctx values as two separate loaded
	// instances, and by default only keeps one resident at a time - a
	// coding call asking for the 32768 window evicts the 8192 instance
	// every other desktop's ordinary chat turn uses, and the next ordinary
	// turn evicts it right back (measured: 161s each way). Raising
	// OLLAMA_MAX_LOADED_MODELS lets both stay loaded at once instead of
	// trading places, but that only pays off if the rarely-used one
	// actually lets go of its memory promptly once idle - so a call that
	// asks for a big NumCtx should also ask for a short KeepAlive, rather
	// than pinning a 32768-context instance resident for the same 30
	// minutes an ordinary chat turn earns.
	KeepAlive string
	// Stop sequence list to truncate generation early when model finishes structured output
	Stop []string
}

type Provider interface {
	Name() string
	Chat(ctx context.Context, model string, messages []Message, opts Options) (Result, error)
	IsReachable(ctx context.Context) bool
}

// ContentFiltered reports whether an error is a provider's content filter refusing the request.
func ContentFiltered(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "content management policy") || strings.Contains(msg, "content_filter") ||
		strings.Contains(msg, "responsibleaipolicyviolation")
}

func hasImages(messages []Message) bool {
	for _, m := range messages {
		if len(m.Images) > 0 {
			return true
		}
	}
	return false
}

// WithoutImages is the messages with every picture taken out and named in its message's text, saying why.
func WithoutImages(messages []Message, why string) []Message {
	out := make([]Message, len(messages))
	for i, m := range messages {
		out[i] = Message{Role: m.Role, Content: m.Content}
		for j := range m.Images {
			what := "a picture"
			if label := m.ImageLabel(j); label != "" {
				what = "the picture of " + label
			}
			out[i].Content += fmt.Sprintf("\n[%s was left out: %s. Read the file as text (office read) if you need it.]", what, why)
		}
	}
	return out
}
