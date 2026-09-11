package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type OllamaProvider struct {
	Host   string
	Client *http.Client
	// EmbedModel is used whenever Embed() is called with an empty model
	// string - lets callers (rag.go, feedback.go) stay provider-agnostic
	// rather than hardcoding "nomic-embed-text" themselves, which would be
	// wrong for any other Embedder implementation (see OpenAIProvider).
	EmbedModel string
}

func NewOllama(host string) *OllamaProvider {
	return &OllamaProvider{Host: host, Client: &http.Client{}, EmbedModel: "nomic-embed-text"}
}

func (o *OllamaProvider) Name() string { return "ollama" }

type ollamaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ollamaChatRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Stream   bool            `json:"stream"`
	Options  map[string]any  `json:"options,omitempty"`
}

type ollamaChatResponse struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
	PromptEvalCount int    `json:"prompt_eval_count"`
	EvalCount       int    `json:"eval_count"`
	TotalDuration   int64  `json:"total_duration"`
	Error           string `json:"error"`
}

// Chat makes a real, blocking call to Ollama's /api/chat. num_ctx is
// forced explicitly - Ollama defaults to 4096 and silently truncates
// context otherwise, a real failure mode discovered building the Node
// prototype this is replacing.
func (o *OllamaProvider) Chat(ctx context.Context, model string, messages []Message, opts Options) (Result, error) {
	timeout := 120 * time.Second
	if opts.TimeoutMs > 0 {
		timeout = time.Duration(opts.TimeoutMs) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	msgs := make([]ollamaMessage, len(messages))
	for i, m := range messages {
		msgs[i] = ollamaMessage{Role: m.Role, Content: m.Content}
	}

	ollamaOpts := map[string]any{"num_ctx": 8192}
	if opts.Temperature > 0 {
		ollamaOpts["temperature"] = opts.Temperature
	}
	reqBody := ollamaChatRequest{
		Model:    model,
		Messages: msgs,
		Stream:   false,
		Options:  ollamaOpts,
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

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return Result{}, fmt.Errorf("ollama: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("ollama returned HTTP %d: %s", resp.StatusCode, string(respBytes))
	}

	var parsed ollamaChatResponse
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return Result{}, fmt.Errorf("ollama: parse response: %w (body: %s)", err, string(respBytes))
	}
	if parsed.Error != "" {
		return Result{}, fmt.Errorf("ollama error: %s", parsed.Error)
	}

	return Result{
		Text:           parsed.Message.Content,
		PromptTokens:   parsed.PromptEvalCount,
		ResponseTokens: parsed.EvalCount,
		DurationMs:     parsed.TotalDuration / 1_000_000,
	}, nil
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
