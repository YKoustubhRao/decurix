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

// AnthropicProvider talks to the real Claude Messages API. Anthropic has
// NO embeddings API at all - this type deliberately does not implement
// Embedder; a Data RAG running with Anthropic as the chat brain needs a
// separate embedder (Ollama or OpenAI), wired in cmd/soul/main.go.
const anthropicDefaultBaseURL = "https://api.anthropic.com"

type AnthropicProvider struct {
	APIKey string
	Client *http.Client
	// BaseURL defaults to the real API - overridable (e.g. in tests,
	// pointed at an httptest.Server) rather than hardcoded inline, the
	// same seam OllamaProvider.Host already provides.
	BaseURL string
	// healthCheckModel is whatever model the caller actually configured
	// for chat - IsReachable uses this rather than a hardcoded guess, so
	// a stale/wrong hardcoded model name can never make a valid key+model
	// combination falsely report unreachable.
	healthCheckModel string
}

func NewAnthropic(apiKey, defaultModel string) *AnthropicProvider {
	return &AnthropicProvider{APIKey: apiKey, Client: &http.Client{}, BaseURL: anthropicDefaultBaseURL, healthCheckModel: defaultModel}
}

func (a *AnthropicProvider) Name() string { return "anthropic" }

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	Temperature float64            `json:"temperature,omitempty"`
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Chat calls Anthropic's real Messages API. A leading "system"-role
// message is pulled out into the top-level `system` field - Anthropic's
// API has no "system" role inside the messages array, unlike Ollama/OpenAI.
func (a *AnthropicProvider) Chat(ctx context.Context, model string, messages []Message, opts Options) (Result, error) {
	timeout := 120 * time.Second
	if opts.TimeoutMs > 0 {
		timeout = time.Duration(opts.TimeoutMs) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var system string
	msgs := make([]anthropicMessage, 0, len(messages))
	for _, m := range messages {
		if m.Role == "system" && system == "" {
			system = m.Content
			continue
		}
		msgs = append(msgs, anthropicMessage{Role: m.Role, Content: m.Content})
	}

	maxTokens := 4096
	if opts.MaxTokens > 0 {
		maxTokens = opts.MaxTokens
	}
	reqBody := anthropicRequest{Model: model, MaxTokens: maxTokens, System: system, Messages: msgs, Temperature: opts.Temperature}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return Result{}, fmt.Errorf("anthropic: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.BaseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("anthropic: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", a.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := a.Client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("anthropic: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return Result{}, fmt.Errorf("anthropic: read response: %w", err)
	}

	var parsed anthropicResponse
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return Result{}, fmt.Errorf("anthropic: parse response: %w (body: %s)", err, string(respBytes))
	}
	if resp.StatusCode != http.StatusOK {
		msg := string(respBytes)
		if parsed.Error != nil {
			msg = parsed.Error.Message
		}
		return Result{}, fmt.Errorf("anthropic returned HTTP %d: %s", resp.StatusCode, msg)
	}

	text := ""
	for _, c := range parsed.Content {
		if c.Type == "text" {
			text += c.Text
		}
	}

	return Result{
		Text:           text,
		PromptTokens:   parsed.Usage.InputTokens,
		ResponseTokens: parsed.Usage.OutputTokens,
	}, nil
}

func (a *AnthropicProvider) IsReachable(ctx context.Context) bool {
	if a.APIKey == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	// Anthropic has no cheap health-check endpoint - a minimal real call
	// against the actually-configured model is the honest way to confirm
	// the key/network/model all really work, not just that a key string
	// is non-empty.
	_, err := a.Chat(ctx, a.healthCheckModel, []Message{{Role: "user", Content: "hi"}}, Options{TimeoutMs: 8000})
	return err == nil
}
