// Package backend talks to the programs that run models: an Ollama server, an
// OpenAI-compatible server (Brainiac's own llama-server, vLLM, Azure, or
// Decurix's control-plane proxy to Claude and GPT), and nothing else. Each
// speaks its own wire format; above this package every model is a Chatter or
// an Embedder.
//
// Depends on nothing internal.
package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Image is one picture shown with a message.
type Image struct {
	Data  []byte
	Mime  string // image/png, image/jpeg, image/webp, image/gif
	Label string // what it is ("invoice.png"), shown next to it where possible
}

// Message is one turn of a conversation.
type Message struct {
	Role   string // system | user | assistant
	Text   string
	Images []Image
}

// ChatRequest is one model call.
type ChatRequest struct {
	Model    string
	Messages []Message
	// MaxTokens caps the answer; 0 lets the backend decide.
	MaxTokens int
	// Temperature is sent only when set; hosted reasoning models refuse it.
	Temperature *float64
	// JSON asks for one JSON object as the answer.
	JSON bool
	Stop []string
	// OnDelta receives the answer as it is written; nil means no streaming.
	OnDelta func(text string)
	// OnThink receives a reasoning model's thought as it is written (it is
	// not part of the answer); for progress only.
	OnThink func(text string)
	// Think lets a thinking model think first (Ollama "think"); off by
	// default, because on a CPU a thought costs as much as an answer.
	Think bool
}

// ChatResult is what came back.
type ChatResult struct {
	Text             string
	PromptTokens     int
	CompletionTokens int
	CachedTokens     int
	// Truncated means the backend stopped for length: the text is real but
	// cut off. A clean 200 with half an answer looks like an answer.
	Truncated bool
	// Complete means the backend said the answer ended: a finish_reason or
	// [DONE] on a stream, Ollama's done, or a whole JSON answer. Chat returns
	// an error instead of an incomplete result, so a nil error comes with
	// Complete; callers check it anyway, so a Chatter that forgets cannot
	// pass a cut answer off as a whole one.
	Complete bool
	Duration time.Duration
	// Model is the model the backend says answered.
	Model string
	// Timings is what a llama-server says the call took; nil from others.
	Timings *Timings
}

// Timings is llama-server's account of one call: how many prompt tokens
// came from its cache and how many it read, how long reading and writing
// took, and, with speculative decoding, how many drafted tokens it kept.
type Timings struct {
	CacheN        int     `json:"cache_n"`
	PromptN       int     `json:"prompt_n"`
	PromptMs      float64 `json:"prompt_ms"`
	PredictedN    int     `json:"predicted_n"`
	PredictedMs   float64 `json:"predicted_ms"`
	DraftN        int     `json:"draft_n,omitempty"`
	DraftAccepted int     `json:"draft_n_accepted,omitempty"`
}

// Chatter is anything that answers a conversation.
type Chatter interface {
	Chat(ctx context.Context, req ChatRequest) (ChatResult, error)
}

// Embedder turns text into vectors.
type Embedder interface {
	Embed(ctx context.Context, model string, inputs []string) ([][]float32, error)
}

// ErrCutShort is an answer stream that ended before the backend said it was
// finished: the server died, was killed, or the connection dropped. The text
// so far is not an answer. It wraps io.ErrUnexpectedEOF.
var ErrCutShort = fmt.Errorf("the answer stream ended before the model finished: %w", io.ErrUnexpectedEOF)

// HTTPError is a backend's answer with a status other than 200.
type HTTPError struct {
	Backend string
	Status  int
	Message string
}

// Error is the failure as "<backend> returned HTTP <status>: <message>".
func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s returned HTTP %d: %s", e.Backend, e.Status, e.Message)
}

// Transient reports whether asking again can cure an error: a deadline, a
// network failure, or a 408/429/5xx. Decided by type, never by wording.
func Transient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var h *HTTPError
	if errors.As(err, &h) {
		return h.Status == http.StatusRequestTimeout || h.Status == http.StatusTooManyRequests || h.Status >= 500
	}
	var n net.Error
	return errors.As(err, &n)
}

// readError turns a failed response into an HTTPError carrying the body's
// error message when it has one (never more than 2 KB of it).
func readError(name string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	msg := strings.TrimSpace(string(body))
	if m := jsonErrorMessage(body); m != "" {
		msg = m
	}
	return &HTTPError{Backend: name, Status: resp.StatusCode, Message: msg}
}

// ImageMime is a picture's media type read from its first bytes, or "".
func ImageMime(b []byte) string {
	switch {
	case len(b) >= 8 && string(b[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "image/jpeg"
	case len(b) >= 6 && (string(b[:6]) == "GIF87a" || string(b[:6]) == "GIF89a"):
		return "image/gif"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}
