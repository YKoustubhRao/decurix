package llm

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`
	// System is the system prompt: a string, or one text block marked for the prompt cache (markForCache); nil for
	// none.
	System      any                `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	Temperature float64            `json:"temperature,omitempty"`
}

// anthropicText is a text block; CacheControl marks the end of a prefix Claude may cache.
type anthropicText struct {
	Type         string          `json:"type"`
	Text         string          `json:"text"`
	CacheControl *anthropicCache `json:"cache_control,omitempty"`
}

// anthropicCache is a cache breakpoint: "ephemeral", Claude's five-minute cache.
type anthropicCache struct {
	Type string `json:"type"`
}

// cacheBreakpointMessages is how many of the newest messages end a cacheable prefix, beside the system prompt: four
// breakpoints in all, the most the API takes. A rolling three, not the last alone, because the operator rewrites the
// newest message (a note appended to it) and its maintenance rewrites older ones: a prefix ending one or two messages
// earlier is then still found (Hermes agent/prompt_caching.py, "system_and_3").
const cacheBreakpointMessages = 3

// markForCache puts Claude's cache breakpoints on a request: the system prompt and the last cacheBreakpointMessages
// messages. A one-shot call (no earlier turn: one user message) gets none: writing a cache costs more than reading the
// prompt once, and nothing would read it. The marks change no text; the same messages always give the same bytes.
func markForCache(req *anthropicRequest) {
	if len(req.Messages) <= 1 {
		return
	}
	if s, ok := req.System.(string); ok && s != "" {
		req.System = []anthropicText{{Type: "text", Text: s, CacheControl: &anthropicCache{Type: "ephemeral"}}}
	}
	for i := max(0, len(req.Messages)-cacheBreakpointMessages); i < len(req.Messages); i++ {
		switch c := req.Messages[i].Content.(type) {
		case string:
			if c != "" {
				req.Messages[i].Content = []anthropicText{{Type: "text", Text: c, CacheControl: &anthropicCache{Type: "ephemeral"}}}
			}
		case []map[string]any:
			// A message with pictures ends with its text block; an empty text cannot carry a mark.
			if n := len(c); n > 0 && (c[n-1]["type"] != "text" || c[n-1]["text"] != "") {
				c[n-1]["cache_control"] = &anthropicCache{Type: "ephemeral"}
			}
		}
	}
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	// StopReason is why the model stopped: "max_tokens" means the answer was cut off.
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		// The prompt read from Claude's cache, and the prompt written to it: both are prompt, beside InputTokens.
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
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

	var systemParts []string
	msgs := make([]anthropicMessage, 0, len(messages))
	for _, m := range messages {
		// Anthropic has no "system" role inside messages[]: every system-role message is pulled into the top-level
		// `system` field. A second one used to fall through and be sent as a messages[] entry with role "system",
		// which the API rejects (review 2026-11-02). They are joined in order instead.
		if m.Role == "system" {
			if strings.TrimSpace(m.Content) != "" {
				systemParts = append(systemParts, m.Content)
			}
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
	reqBody := anthropicRequest{Model: model, MaxTokens: maxTokens, Messages: msgs, Temperature: opts.Temperature}
	if system := strings.Join(systemParts, "\n\n"); system != "" {
		reqBody.System = system
	}
	markForCache(&reqBody)
	var parsed anthropicResponse
	headers := map[string]string{"x-api-key": a.APIKey, "anthropic-version": "2023-06-01"}
	// Sent through rateLimitedRequest, as openai.go's calls are: a 429 or the provider's own failure (5xx, 529
	// overloaded) is waited out as its retry-after says, any other refusal is final.
	do := func(req *http.Request) (*http.Response, error) { return rateLimitedRequest(a.Client, req) }
	if _, err := postJSON(ctx, do, "anthropic", a.BaseURL+"/v1/messages", headers, reqBody, &parsed, &parsed.apiError); err != nil {
		return Result{}, err
	}

	text := ""
	for _, c := range parsed.Content {
		if c.Type == "text" {
			text += c.Text
		}
	}

	u := parsed.Usage
	return Result{
		Text:           text,
		PromptTokens:   u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens,
		ResponseTokens: u.OutputTokens,
		CachedTokens:   u.CacheReadInputTokens,
		Truncated:      parsed.StopReason == "max_tokens",
		FinishReason:   parsed.StopReason,
	}, nil
}

// Endpoint is where this provider sends its calls (RouteOf).
func (a *AnthropicProvider) Endpoint() string { return a.BaseURL }

// IsReachable asks the Models API about the configured model (GET /v1/models/{model}): the key, the network and the
// model, confirmed without a paid call. It used to send a real chat message, and /api/health (polled every few
// seconds while the UI is open) paid for one each time (review T-25).
func (a *AnthropicProvider) IsReachable(ctx context.Context) bool {
	if a.APIKey == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	target := a.BaseURL + "/v1/models"
	if a.healthCheckModel != "" {
		target += "/" + url.PathEscape(a.healthCheckModel)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return false
	}
	req.Header.Set("x-api-key", a.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := a.Client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
