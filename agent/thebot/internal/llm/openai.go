package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
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
	// Effort is the reasoning effort asked of the model ("" leaves it to the model's default).
	Effort string
}

// reasoningEffort is what a person set for this worker in ~/.decurix/reasoning-effort: low, medium or high; "" otherwise.
func reasoningEffort() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".decurix", "reasoning-effort"))
	if err != nil {
		return ""
	}
	switch effort := strings.TrimSpace(string(data)); effort {
	case "low", "medium", "high":
		return effort
	}
	return ""
}

func NewOpenAI(apiKey, embedModel string) *OpenAIProvider {
	if embedModel == "" {
		embedModel = "text-embedding-3-small"
	}
	return &OpenAIProvider{APIKey: apiKey, Client: &http.Client{}, EmbedModel: embedModel, BaseURL: openAIDefaultBaseURL, Effort: reasoningEffort()}
}

func (o *OpenAIProvider) Name() string { return "openai" }

// authHeader is the header that carries the API key.
func (o *OpenAIProvider) authHeader() map[string]string {
	return map[string]string{"Authorization": "Bearer " + o.APIKey}
}

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
	Effort      string          `json:"reasoning_effort,omitempty"`
	CacheKey    string          `json:"prompt_cache_key,omitempty"`
}

// noCacheKey is set once a provider refused prompt_cache_key: it is not sent again.
var noCacheKey atomic.Bool

// cacheKey routes one goal's calls of one kind to the same cache: its instructions (the first message) and its request
// (the first user message), hashed. Found (2026-10-31): with no key, GPT-6 deployments cached 35% of the prompt against
// 83% on gpt-5.4, with the same harness.
func cacheKey(msgs []openaiMessage) string {
	if noCacheKey.Load() || len(msgs) == 0 {
		return ""
	}
	h := sha256.New()
	fmt.Fprint(h, msgs[0].Content)
	for _, m := range msgs[1:] {
		if m.Role == "user" {
			fmt.Fprint(h, m.Content)
			break
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

type openaiChatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		// FinishReason is why the model stopped: "length" (or a proxied "max_tokens") means the answer was cut off.
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	apiError
}

// Chat sends one chat request. A request the provider's content filter refuses while it carries pictures is sent
// once more with the pictures left out, each named in the text: found live (2026-10-14), a scanned identity document
// in an onboarding task made Azure answer "filtered due to the prompt triggering ... content management policy" and
// the whole task ended in error. The model can still read the document as text.
func (o *OpenAIProvider) Chat(ctx context.Context, model string, messages []Message, opts Options) (Result, error) {
	res, err := o.chatOnce(ctx, model, messages, opts)
	if err == nil || !hasImages(messages) {
		return res, err
	}
	switch {
	case ContentFiltered(err):
		return o.chatOnce(ctx, model, WithoutImages(messages, "the model provider's content filter refused it"), opts)
	case ImageRefused(err):
		// A picture the provider cannot read (2026-10-22) never ends the task: the rest is sent without it.
		return o.chatOnce(ctx, model, WithoutImages(messages, "the model provider could not read it as a picture"), opts)
	}
	return res, err
}

func (o *OpenAIProvider) chatOnce(ctx context.Context, model string, messages []Message, opts Options) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, opts.timeout())
	defer cancel()

	msgs := make([]openaiMessage, len(messages))
	for i, m := range messages {
		msgs[i] = openaiMessage{Role: m.Role, Content: m.Content}
		if len(m.Images) > 0 {
			parts := []map[string]any{{"type": "text", "text": m.Content}}
			for j, img := range base64All(m.Images) {
				kind := ImageType(m.Images[j])
				if kind == "" {
					parts = append(parts, map[string]any{"type": "text", "text": notAPicture(j+1, m.ImageLabel(j))})
					continue
				}
				if label := m.ImageLabel(j); label != "" {
					parts = append(parts, map[string]any{"type": "text", "text": "Picture " + strconv.Itoa(j+1) + ": " + label})
				}
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{"url": "data:" + kind + ";base64," + img}})
			}
			msgs[i].Content = parts
		}
	}

	reqBody := openaiChatRequest{
		Model:       model,
		Messages:    msgs,
		Temperature: opts.Temperature,
		MaxTokens:   opts.MaxTokens,
		Stop:        opts.Stop,
		Effort:      o.Effort,
		CacheKey:    cacheKey(msgs),
	}
	do := func(req *http.Request) (*http.Response, error) { return rateLimitedRequest(o.Client, req) }
	var parsed openaiChatResponse
	respBytes, err := postJSON(ctx, do, "openai", o.BaseURL+"/v1/chat/completions", o.authHeader(), reqBody, &parsed, &parsed.apiError)
	if err != nil && reqBody.CacheKey != "" && strings.Contains(err.Error(), "prompt_cache_key") {
		// A provider that does not know the key: the same request without it, and never again with it.
		noCacheKey.Store(true)
		reqBody.CacheKey, parsed = "", openaiChatResponse{}
		respBytes, err = postJSON(ctx, do, "openai", o.BaseURL+"/v1/chat/completions", o.authHeader(), reqBody, &parsed, &parsed.apiError)
	}
	if err != nil {
		return Result{}, err
	}
	if len(parsed.Choices) == 0 {
		return Result{}, fmt.Errorf("openai: response contained no choices (body: %s)", string(respBytes))
	}

	return Result{
		Text:           parsed.Choices[0].Message.Content,
		PromptTokens:   parsed.Usage.PromptTokens,
		ResponseTokens: parsed.Usage.CompletionTokens,
		CachedTokens:   parsed.Usage.PromptTokensDetails.CachedTokens,
		Truncated:      truncatedFinish(parsed.Choices[0].FinishReason),
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
	apiError
}

// Embed calls OpenAI's real /v1/embeddings endpoint.
func (o *OpenAIProvider) Embed(ctx context.Context, model string, text string) ([]float32, error) {
	if model == "" {
		model = o.EmbedModel
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	var parsed openaiEmbedResponse
	if _, err := postJSON(ctx, o.Client.Do, "openai embed", o.BaseURL+"/v1/embeddings", o.authHeader(), openaiEmbedRequest{Model: model, Input: text}, &parsed, &parsed.apiError); err != nil {
		return nil, err
	}
	if len(parsed.Data) == 0 {
		return nil, fmt.Errorf("openai embed: response contained no data")
	}
	return parsed.Data[0].Embedding, nil
}

// truncatedFinish reports whether an OpenAI-style finish_reason says the answer was cut off for room ("length"; a
// proxy in front of another provider may pass on its "max_tokens").
func truncatedFinish(reason string) bool {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "length", "max_tokens", "max_output_tokens":
		return true
	}
	return false
}
