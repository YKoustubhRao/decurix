package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// defaultTimeoutFloor is the shortest any chat call may be given, no
// matter what the call site asked for. Override with OLLAMA_TIMEOUT_MS.
const defaultTimeoutFloor = 300 * time.Second

// defaultKeepAlive is how long Ollama is asked to hold the model in
// memory after answering. Override with OLLAMA_KEEP_ALIVE.
const defaultKeepAlive = "60m"

type OllamaProvider struct {
	Host   string
	Client *http.Client
	// EmbedModel is used whenever Embed() is called with an empty model
	// string - lets callers (rag.go, feedback.go) stay provider-agnostic
	// rather than hardcoding "nomic-embed-text" themselves, which would be
	// wrong for any other Embedder implementation (see OpenAIProvider).
	EmbedModel string

	// TimeoutFloor raises any per-call timeout shorter than itself.
	//
	// Every Options.TimeoutMs in this repo - 60s to plan, 90s to answer,
	// 120s to write a file - was picked against a GPU. The same code now
	// runs against whatever Ollama the host happens to have, which is
	// routinely CPU-only, and there the cost is not generation but
	// reading the prompt: measured on an ordinary laptop, a 3B model
	// evaluates ~14 tokens/sec, so a 600-token prompt spends ~45 seconds
	// before producing a single token. Those budgets then expire in
	// sequence - classify, plan, answer, reason about why answering
	// failed - and what reaches the human is "could not reason about the
	// failure (... context deadline exceeded) - stopping here" for
	// asking the desktop its own name.
	//
	// A floor, never a replacement: a call site that asks for longer
	// (a coding build asks for 240s) keeps exactly what it asked for.
	TimeoutFloor time.Duration

	// KeepAlive is how long Ollama holds this model in memory after
	// answering. Its own default is 5 minutes, and evicting the model
	// also throws away the evaluated prompt prefix cached alongside it,
	// so a job that pauses to run a command comes back to a cold model.
	// Measured on the same laptop: 31.8s to read the agent's system
	// prompt cold, 0.6s with the model still resident.
	KeepAlive string
}

func NewOllama(host string) *OllamaProvider {
	return &OllamaProvider{
		Host:         host,
		Client:       &http.Client{},
		EmbedModel:   getenvDefault("EMBED_MODEL", "nomic-embed-text"),
		TimeoutFloor: time.Duration(EnvInt("OLLAMA_TIMEOUT_MS", int(defaultTimeoutFloor/time.Millisecond))) * time.Millisecond,
		KeepAlive:    getenvDefault("OLLAMA_KEEP_ALIVE", defaultKeepAlive),
	}
}

func getenvDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func defaultNumThread() int {
	cpus := runtime.NumCPU()
	if cpus > 4 {
		return cpus - 2
	}
	if cpus > 2 {
		return cpus - 1
	}
	return 1
}

// EnvInt reads a positive whole number from the environment, or fallback when it is unset, unparseable or not positive.
func EnvInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func (o *OllamaProvider) Name() string { return "ollama" }

type ollamaMessage struct {
	Role    string   `json:"role"`
	Content string   `json:"content"`
	Images  []string `json:"images,omitempty"` // base64, for a vision model
}

type ollamaChatRequest struct {
	Model     string          `json:"model"`
	Messages  []ollamaMessage `json:"messages"`
	Stream    bool            `json:"stream"`
	Options   map[string]any  `json:"options,omitempty"`
	KeepAlive string          `json:"keep_alive,omitempty"`
}

type ollamaChatResponse struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
	PromptEvalCount int    `json:"prompt_eval_count"`
	EvalCount       int    `json:"eval_count"`
	TotalDuration   int64  `json:"total_duration"`
	Error           string `json:"error"`
	// Done marks the final chunk of a stream, which is the only one
	// carrying the real token counters and the stop reason.
	Done bool `json:"done"`
	// DoneReason is how Ollama says WHY it stopped. "length" means it hit
	// the token ceiling and the content is cut off mid-sentence - the
	// response is still a clean HTTP 200, so without reading this there is
	// no way to tell a truncated file from a model that writes bad code.
	DoneReason string `json:"done_reason"`
}

// Chat makes a real, blocking call to Ollama's /api/chat. num_ctx is
// forced explicitly - Ollama defaults to 4096 and silently truncates
// context otherwise, a real failure mode discovered building the Node
// prototype this is replacing.
func (o *OllamaProvider) Chat(ctx context.Context, model string, messages []Message, opts Options) (Result, error) {
	timeout := opts.timeout()
	if o.TimeoutFloor > timeout {
		timeout = o.TimeoutFloor
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	msgs := make([]ollamaMessage, len(messages))
	for i, m := range messages {
		msgs[i] = ollamaMessage{Role: m.Role, Content: m.LabelledContent(), Images: base64All(onlyPictures(m.Images))}
	}

	// 8192 stays the default for ordinary calls; a caller that genuinely
	// needs more (code generation - see Options.NumCtx) asks for it
	// explicitly rather than every chat turn paying for a huge window.
	numCtx := 8192
	if opts.NumCtx > 0 {
		numCtx = opts.NumCtx
	}
	ollamaOpts := map[string]any{
		"num_ctx":    numCtx,
		"num_thread": EnvInt("OLLAMA_NUM_THREAD", defaultNumThread()),
		"num_batch":  EnvInt("OLLAMA_NUM_BATCH", 512),
	}
	if opts.Temperature > 0 {
		ollamaOpts["temperature"] = opts.Temperature
	}
	if opts.MaxTokens > 0 {
		ollamaOpts["num_predict"] = opts.MaxTokens
	}
	if len(opts.Stop) > 0 {
		ollamaOpts["stop"] = opts.Stop
	}
	// Streaming, always - even though every caller here wants one complete
	// answer and nothing renders tokens as they arrive.
	//
	// The reason is the network in between, not the API. A non-streaming
	// call holds the connection open with ZERO bytes sent until the whole
	// answer is ready, and any proxy on the path reads that silence as a
	// dead origin. Found live: a real 24B model generating a real source
	// file through a Cloudflare tunnel took several minutes, and the tunnel
	// killed it at 100 seconds with HTTP 524 every single time - the model
	// was working fine, nothing ever got to see it. Streaming keeps real
	// bytes flowing the whole time, so an idle timeout never fires, and the
	// accumulated result is identical.
	keepAlive := o.KeepAlive
	if opts.KeepAlive != "" {
		keepAlive = opts.KeepAlive
	}

	reqBody := ollamaChatRequest{
		Model:     model,
		Messages:  msgs,
		Stream:    true,
		Options:   ollamaOpts,
		KeepAlive: keepAlive,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return Result{}, fmt.Errorf("ollama: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.Host+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("ollama: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.Client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("ollama: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return Result{}, fmt.Errorf("ollama returned HTTP %d: %s", resp.StatusCode, string(respBytes))
	}

	return readOllamaStream(resp.Body)
}

// readOllamaStream accumulates Ollama's newline-delimited JSON stream into
// one complete result. Each line is a chunk carrying the next fragment of
// content; the final one carries done=true plus the real counters.
func readOllamaStream(r io.Reader) (Result, error) {
	var text strings.Builder
	var final ollamaChatResponse
	sawDone := false

	scanner := bufio.NewScanner(r)
	// A single chunk is tiny, but the default 64KB token limit is not worth
	// betting a whole build on - a long line here would silently truncate
	// the answer, which is the exact class of invisible failure this
	// package has already been bitten by once.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var chunk ollamaChatResponse
		if err := json.Unmarshal(line, &chunk); err != nil {
			return Result{}, fmt.Errorf("ollama: parse stream chunk: %w (chunk: %s)", err, truncateForError(string(line)))
		}
		if chunk.Error != "" {
			return Result{}, fmt.Errorf("ollama error: %s", chunk.Error)
		}
		text.WriteString(chunk.Message.Content)
		if chunk.Done {
			final, sawDone = chunk, true
		}
	}
	if err := scanner.Err(); err != nil {
		return Result{}, fmt.Errorf("ollama: read stream: %w", err)
	}
	if !sawDone {
		// The stream ended without its final chunk: the connection dropped or the server stopped mid-answer. What came
		// is not the whole answer, and is never taken for one.
		return Result{Text: text.String(), Truncated: true},
			fmt.Errorf("ollama: the answer stream ended before it was finished: %w", io.ErrUnexpectedEOF)
	}

	return Result{
		Text:           text.String(),
		PromptTokens:   final.PromptEvalCount,
		ResponseTokens: final.EvalCount,
		DurationMs:     final.TotalDuration / 1_000_000,
		Truncated:      final.DoneReason == "length",
	}, nil
}

func truncateForError(s string) string {
	if len(s) <= 300 {
		return s
	}
	return s[:300] + "..."
}

func (o *OllamaProvider) IsReachable(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.Host+"/api/tags", nil)
	if err != nil {
		return false
	}
	resp, err := o.Client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
