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

const openAIDefaultBaseURL = "https://api.openai.com"

// OpenAIProvider talks to the real Chat Completions API and, unlike
// Anthropic, ALSO implements Embedder - OpenAI has a real embeddings
// endpoint, so choosing OpenAI as the chat brain needs no separate
// embedder for the Data RAG (Ollama and Anthropic don't share that
// property; Anthropic has no embeddings API at all).
type OpenAIProvider struct {
	APIKey     string
	Client     *http.Client
	EmbedModel string // e.g. "text-embedding-3-small"
	// BaseURL defaults to the real API - overridable in tests, the same
	// seam OllamaProvider.Host and AnthropicProvider.BaseURL provide.
	BaseURL string
}

func NewOpenAI(apiKey, embedModel string) *OpenAIProvider {
	if embedModel == "" {
		embedModel = "text-embedding-3-small"
	}
	return &OpenAIProvider{APIKey: apiKey, Client: &http.Client{}, EmbedModel: embedModel, BaseURL: openAIDefaultBaseURL}
}

func (o *OpenAIProvider) Name() string { return "openai" }

type openaiMessage struct {
	Role string `json:"role"`
	// Content is a string, or a list of text and image_url parts when the
	// message carries images.
	Content any `json:"content"`
}

type openaiChatRequest struct {
	Model       string          `json:"model"`
	Messages    []openaiMessage `json:"messages"`
	Temperature float64         `json:"temperature,omitempty"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Stop        []string        `json:"stop,omitempty"`
}

type openaiChatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (o *OpenAIProvider) Chat(ctx context.Context, model string, messages []Message, opts Options) (Result, error) {
	timeout := 120 * time.Second
	if opts.TimeoutMs > 0 {
		timeout = time.Duration(opts.TimeoutMs) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	msgs := make([]openaiMessage, len(messages))
	for i, m := range messages {
		msgs[i] = openaiMessage{Role: m.Role, Content: m.Content}
		if len(m.Images) > 0 {
			parts := []map[string]any{{"type": "text", "text": m.Content}}
			for _, img := range base64All(m.Images) {
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{"url": "data:image/png;base64," + img}})
			}
			msgs[i].Content = parts
		}
	}

	body, err := json.Marshal(openaiChatRequest{
		Model:       model,
		Messages:    msgs,
		Temperature: opts.Temperature,
		MaxTokens:   opts.MaxTokens,
		Stop:        opts.Stop,
	})
	if err != nil {
		return Result{}, fmt.Errorf("openai: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("openai: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.APIKey)

	resp, err := rateLimitedRequest(o.Client, req)
	if err != nil {
		return Result{}, fmt.Errorf("openai: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return Result{}, fmt.Errorf("openai: read response: %w", err)
	}

	var parsed openaiChatResponse
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return Result{}, fmt.Errorf("openai: parse response: %w (body: %s)", err, string(respBytes))
	}
	if resp.StatusCode != http.StatusOK {
		msg := string(respBytes)
		if parsed.Error != nil {
			msg = parsed.Error.Message
		}
		return Result{}, fmt.Errorf("openai returned HTTP %d: %s", resp.StatusCode, msg)
	}
	if len(parsed.Choices) == 0 {
		return Result{}, fmt.Errorf("openai: response contained no choices (body: %s)", string(respBytes))
	}

	return Result{
		Text:           parsed.Choices[0].Message.Content,
		PromptTokens:   parsed.Usage.PromptTokens,
		ResponseTokens: parsed.Usage.CompletionTokens,
	}, nil
}

func (o *OpenAIProvider) IsReachable(ctx context.Context) bool {
	if o.APIKey == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.BaseURL+"/v1/models", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+o.APIKey)
	resp, err := o.Client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

type openaiEmbedRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type openaiEmbedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Embed calls OpenAI's real /v1/embeddings endpoint.
func (o *OpenAIProvider) Embed(ctx context.Context, model string, text string) ([]float32, error) {
	if model == "" {
		model = o.EmbedModel
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	body, err := json.Marshal(openaiEmbedRequest{Model: model, Input: text})
	if err != nil {
		return nil, fmt.Errorf("openai embed: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai embed: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.APIKey)

	resp, err := o.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai embed: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("openai embed: read response: %w", err)
	}

	var parsed openaiEmbedResponse
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return nil, fmt.Errorf("openai embed: parse response: %w (body: %s)", err, string(respBytes))
	}
	if resp.StatusCode != http.StatusOK {
		msg := string(respBytes)
		if parsed.Error != nil {
			msg = parsed.Error.Message
		}
		return nil, fmt.Errorf("openai embed returned HTTP %d: %s", resp.StatusCode, msg)
	}
	if len(parsed.Data) == 0 {
		return nil, fmt.Errorf("openai embed: response contained no data")
	}
	return parsed.Data[0].Embedding, nil
}
