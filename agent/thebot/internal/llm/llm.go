// Package llm defines "the brain" as a pluggable interface. Ollama is the
// only concrete implementation for now (local-first, zero-config
// default); OpenAI/Anthropic/etc. plug in later as bring-your-own-key,
// opt-in providers without anything above this interface (atomizer,
// scheduler, skills) needing to change - they only ever see Provider.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
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
	// FinishReason is why the provider says it stopped, as it said it ("stop", "length", "end_turn", ...), or
	// "repetition" when soul cut a stream that had fallen into a loop (runaway.go). "" when it said nothing.
	FinishReason string
}

// RouteKey is one route to a model: the provider, the model asked for and the endpoint it was asked at. A fact soul
// learns about a route while running (a field it refuses, what a picture costs) is kept under the whole key, so a
// change of brain or of endpoint starts from nothing rather than inheriting another route's fact.
type RouteKey struct {
	Provider string
	Model    string
	Endpoint string
}

// String is the key as "provider/model@endpoint".
func (k RouteKey) String() string { return k.Provider + "/" + k.Model + "@" + k.Endpoint }

// endpointer is a provider that says where it sends its calls.
type endpointer interface{ Endpoint() string }

// unwrapper is a wrapper (Limited, Metered, a trace) that can hand back the provider it wraps.
type unwrapper interface{ Unwrap() Provider }

// RouteOf is the route p's calls for model take: its name, the model, and the endpoint of the first provider under
// any wrappers that says where it sends its calls ("" when none does).
func RouteOf(p Provider, model string) RouteKey {
	key := RouteKey{Model: model}
	if p == nil {
		return key
	}
	key.Provider = p.Name()
	for depth := 0; p != nil && depth < 8; depth++ {
		if e, ok := p.(endpointer); ok {
			key.Endpoint = e.Endpoint()
			break
		}
		u, ok := p.(unwrapper)
		if !ok {
			break
		}
		p = u.Unwrap()
	}
	return key
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
	// Purpose says what the call is for: one of the Purpose constants (purpose.go). It travels beside the request,
	// in a header (X-Decurix-Purpose) and in soul's trace, never in the request body. Required at every call site
	// (purpose_test.go); a call without one goes out as PurposeUntagged and is logged once per call site.
	Purpose string
	// ResponseFormat is an OpenAI response_format object as JSON ({"type":"json_object"}, or a json_schema one), or ""
	// for none. A string, not raw bytes, so Options stays comparable. The OpenAI provider sends it as it is; Ollama
	// takes it as its "format" (ollamaFormat). Set only by the Python agent's calls through soul (/api/llm/call):
	// soul's own calls leave it empty, so their request bodies are byte-identical to before.
	ResponseFormat string
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

// ImageRefused reports whether an error is a provider refusing a picture it cannot read.
func ImageRefused(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unsupported image") || strings.Contains(msg, "invalid image") ||
		strings.Contains(msg, "could not process image")
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

// timeout is the call's TimeoutMs as a duration, or two minutes when unset.
func (o Options) timeout() time.Duration {
	if o.TimeoutMs > 0 {
		return time.Duration(o.TimeoutMs) * time.Millisecond
	}
	return 120 * time.Second
}

// apiError is the error object a hosted API puts in a failed response.
type apiError struct {
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// postJSON sends in as JSON to url through do and decodes the answer into out; a status other than 200 is an error
// carrying failure's message (out's apiError). Every error starts with name. It returns the raw answer.
func postJSON(ctx context.Context, do func(*http.Request) (*http.Response, error), name, url string, headers map[string]string, in, out any, failure *apiError) ([]byte, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("%s: marshal request: %w", name, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%s: build request: %w", name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: request failed: %w", name, err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s: read response: %w", name, err)
	}
	// The status decides first: a failure is an HTTPError whatever its body holds (JSON or plain text).
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(respBytes))
		if json.Unmarshal(respBytes, out) == nil && failure.Error != nil {
			msg = failure.Error.Message
		}
		return nil, &HTTPError{Provider: name, Status: resp.StatusCode, Message: msg}
	}
	if err := json.Unmarshal(respBytes, out); err != nil {
		return nil, fmt.Errorf("%s: parse response: %w (body: %s)", name, err, string(respBytes))
	}
	return respBytes, nil
}

// HTTPError is a provider's answer with a status other than 200.
type HTTPError struct {
	Provider string
	Status   int
	Message  string
}

// Error is the failure as "<provider> returned HTTP <status>: <message>".
func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s returned HTTP %d: %s", e.Provider, e.Status, e.Message)
}

// Transient reports whether a chat call's error is one asking again can cure - decided by its type, never its
// wording: the call's deadline passed, the network failed, or the provider answered with a temporary failure (408,
// 429, 5xx). A refusal of the request itself (any other 4xx, a parse error) is not.
func Transient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.Status == http.StatusRequestTimeout || httpErr.Status == http.StatusTooManyRequests ||
			httpErr.Status >= 500
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}
