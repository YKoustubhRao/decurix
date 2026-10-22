package llm

import (
	"context"
	"net/http"
	"strconv"
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
	Role string `json:"role"`
	// Content is a string, or text and image blocks when the message carries
	// images.
	Content any `json:"content"`
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
	apiError
}

// Chat calls Anthropic's Messages API; a picture the provider cannot read never ends the task: the call is made once
// more with the pictures left out, each named in the text (as openai.go does).
func (a *AnthropicProvider) Chat(ctx context.Context, model string, messages []Message, opts Options) (Result, error) {
	res, err := a.chatOnce(ctx, model, messages, opts)
	if err != nil && hasImages(messages) && ImageRefused(err) {
		return a.chatOnce(ctx, model, WithoutImages(messages, "the model provider could not read it as a picture"), opts)
	}
	return res, err
}

// chatOnce makes one call to Anthropic's real Messages API. A leading "system"-role
// message is pulled out into the top-level `system` field - Anthropic's
// API has no "system" role inside the messages array, unlike Ollama/OpenAI.
func (a *AnthropicProvider) chatOnce(ctx context.Context, model string, messages []Message, opts Options) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, opts.timeout())
	defer cancel()

	var system string
	msgs := make([]anthropicMessage, 0, len(messages))
	for _, m := range messages {
		if m.Role == "system" && system == "" {
			system = m.Content
			continue
		}
		msg := anthropicMessage{Role: m.Role, Content: m.Content}
		if len(m.Images) > 0 {
			blocks := []map[string]any{}
			for j, img := range base64All(m.Images) {
				kind := ImageType(m.Images[j])
				if kind == "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": notAPicture(j+1, m.ImageLabel(j))})
					continue
				}
				if label := m.ImageLabel(j); label != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": "Picture " + strconv.Itoa(j+1) + ": " + label})
				}
				blocks = append(blocks, map[string]any{"type": "image", "source": map[string]string{"type": "base64", "media_type": kind, "data": img}})
			}
			msg.Content = append(blocks, map[string]any{"type": "text", "text": m.Content})
		}
		msgs = append(msgs, msg)
	}

	maxTokens := 4096
	if opts.MaxTokens > 0 {
		maxTokens = opts.MaxTokens
	}
	reqBody := anthropicRequest{Model: model, MaxTokens: maxTokens, System: system, Messages: msgs, Temperature: opts.Temperature}
	var parsed anthropicResponse
	headers := map[string]string{"x-api-key": a.APIKey, "anthropic-version": "2023-06-01"}
	if _, err := postJSON(ctx, a.Client.Do, "anthropic", a.BaseURL+"/v1/messages", headers, reqBody, &parsed, &parsed.apiError); err != nil {
		return Result{}, err
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
