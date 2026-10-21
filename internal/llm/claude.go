package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

const (
	// DefaultClaudeModel is the model a Claude brain uses when none is given.
	DefaultClaudeModel = "claude-opus-5-5"
	// DefaultClaudeFastModel is the cheaper one asked for with "fast".
	DefaultClaudeFastModel = "claude-haiku-4-5"

	// claudeMaxTokens is the answer cap when the request names none. Kept
	// under what a non-streaming call can finish inside requestTimeout.
	claudeMaxTokens = 16000
)

// Claude answers OpenAI-shaped chat requests with Claude. Everything a desktop
// sends is in the OpenAI shape (TheBot's openai provider, the agent's
// llm_client), so one proxy contract serves both hosted providers; this type
// translates to the Messages API and back.
type Claude struct {
	Key       string
	Model     string
	FastModel string
	// BaseURL overrides the API address (tests); "" is the real API.
	BaseURL string
	HTTP    *http.Client
}

// NewClaude builds a Claude client; empty model names take the defaults.
func NewClaude(key, model, fastModel string) *Claude {
	c := &Claude{
		Key:       strings.TrimSpace(key),
		Model:     strings.TrimSpace(model),
		FastModel: strings.TrimSpace(fastModel),
		HTTP:      &http.Client{Timeout: requestTimeout},
	}

	if c.Model == "" {
		c.Model = DefaultClaudeModel
	}

	if c.FastModel == "" {
		c.FastModel = DefaultClaudeFastModel
	}

	return c
}

// Configured reports whether chats can be answered at all.
func (c *Claude) Configured() bool {
	return c != nil && c.Key != ""
}

// openAIRequest is the part of a chat-completions request this translates.
type openAIRequest struct {
	Model               string           `json:"model"`
	Messages            []openAIMessage  `json:"messages"`
	Tools               []openAITool     `json:"tools"`
	ToolChoice          json.RawMessage  `json:"tool_choice"`
	MaxTokens           int64            `json:"max_tokens"`
	MaxCompletionTokens int64            `json:"max_completion_tokens"`
	Stop                json.RawMessage  `json:"stop"`
	ResponseFormat      *json.RawMessage `json:"response_format"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content"`
	ToolCalls  []openAIToolCall `json:"tool_calls"`
	ToolCallID string           `json:"tool_call_id"`
}

type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAITool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

type openAIPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL struct {
		URL string `json:"url"`
	} `json:"image_url"`
}

// Chat answers one OpenAI-shaped chat request body with Claude and returns an
// OpenAI-shaped answer: the upstream status, the body, and the retry headers
// of a rate limit (RetryHeaders plus Claude's own retry-after).
func (c *Claude) Chat(ctx context.Context, body []byte) (int, []byte, http.Header, error) {
	if !c.Configured() {
		return 0, nil, nil, ErrNotConfigured
	}

	var req openAIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return 0, nil, nil, fmt.Errorf("llm: request is not a chat request: %w", err)
	}

	model, err := resolve(req.Model, c.Model, c.FastModel)
	if err != nil {
		return 0, nil, nil, err
	}

	params, err := toClaudeParams(req, model)
	if err != nil {
		return http.StatusBadRequest, errorBody(err.Error()), nil, nil
	}

	opts := []option.RequestOption{
		option.WithAPIKey(c.Key),
		// The desktop's own client retries, waiting as long as the rate
		// limit says (llm_client.retry_after): retrying here as well would
		// multiply the wait and hide the headers it reads.
		option.WithMaxRetries(0),
		option.WithRequestTimeout(requestTimeout),
	}
	if c.HTTP != nil {
		opts = append(opts, option.WithHTTPClient(c.HTTP))
	}
	if c.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(c.BaseURL))
	}

	client := anthropic.NewClient(opts...)

	msg, err := client.Messages.New(ctx, params)
	if err != nil {
		var apiErr *anthropic.Error
		if errors.As(err, &apiErr) {
			retry := http.Header{}
			if apiErr.Response != nil {
				retry = copyHeaders(apiErr.Response.Header, append([]string{"retry-after"}, RetryHeaders...))
			}

			return apiErr.StatusCode, errorBody(claudeErrorMessage(apiErr)), retry, nil
		}

		return 0, nil, nil, fmt.Errorf("llm: %w", err)
	}

	out, err := json.Marshal(fromClaudeMessage(msg))
	if err != nil {
		return 0, nil, nil, err
	}

	return http.StatusOK, out, http.Header{}, nil
}

// toClaudeParams translates an OpenAI chat request: system/developer
// messages become the system prompt, tool calls and tool results become
// tool_use / tool_result blocks, and consecutive turns of one role are merged
// (the Messages API wants user and assistant to alternate). Sampling
// parameters are dropped - the current models refuse a non-default
// temperature - and a forced tool choice becomes "auto", which is all they
// accept.
func toClaudeParams(req openAIRequest, model string) (anthropic.MessageNewParams, error) {
	params := anthropic.MessageNewParams{Model: anthropic.Model(model)}

	params.MaxTokens = claudeMaxTokens
	if req.MaxCompletionTokens > 0 {
		params.MaxTokens = req.MaxCompletionTokens
	} else if req.MaxTokens > 0 {
		params.MaxTokens = req.MaxTokens
	}

	var system []string

	appendTurn := func(role anthropic.MessageParamRole, blocks ...anthropic.ContentBlockParamUnion) {
		if len(blocks) == 0 {
			return
		}
		if n := len(params.Messages); n > 0 && params.Messages[n-1].Role == role {
			params.Messages[n-1].Content = append(params.Messages[n-1].Content, blocks...)
			return
		}
		params.Messages = append(params.Messages, anthropic.MessageParam{Role: role, Content: blocks})
	}

	for i, m := range req.Messages {
		switch m.Role {
		case "system", "developer":
			text, _, err := contentBlocks(m.Content, false)
			if err != nil {
				return params, fmt.Errorf("message %d: %w", i, err)
			}
			if text != "" {
				system = append(system, text)
			}

		case "user":
			_, blocks, err := contentBlocks(m.Content, true)
			if err != nil {
				return params, fmt.Errorf("message %d: %w", i, err)
			}
			appendTurn(anthropic.MessageParamRoleUser, blocks...)

		case "assistant":
			_, blocks, err := contentBlocks(m.Content, false)
			if err != nil {
				return params, fmt.Errorf("message %d: %w", i, err)
			}
			for _, call := range m.ToolCalls {
				var input any = map[string]any{}
				if strings.TrimSpace(call.Function.Arguments) != "" {
					if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
						input = map[string]any{"arguments": call.Function.Arguments}
					}
				}
				blocks = append(blocks, anthropic.NewToolUseBlock(call.ID, input, call.Function.Name))
			}
			appendTurn(anthropic.MessageParamRoleAssistant, blocks...)

		case "tool":
			text, _, err := contentBlocks(m.Content, false)
			if err != nil {
				return params, fmt.Errorf("message %d: %w", i, err)
			}
			appendTurn(anthropic.MessageParamRoleUser, anthropic.NewToolResultBlock(m.ToolCallID, text, false))

		default:
			return params, fmt.Errorf("message %d: role %q is not supported", i, m.Role)
		}
	}

	if len(params.Messages) == 0 {
		return params, errors.New("the request has no user message")
	}

	if req.ResponseFormat != nil && strings.Contains(string(*req.ResponseFormat), "json") {
		system = append(system, "Reply with a single JSON object and nothing else.")
	}

	for _, s := range system {
		params.System = append(params.System, anthropic.TextBlockParam{Text: s})
	}

	if stops := stopSequences(req.Stop); len(stops) > 0 {
		params.StopSequences = stops
	}

	for _, t := range req.Tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}

		schema := anthropic.ToolInputSchemaParam{}
		if props, ok := t.Function.Parameters["properties"]; ok {
			schema.Properties = props
		}
		if req, ok := t.Function.Parameters["required"].([]any); ok {
			for _, r := range req {
				if name, ok := r.(string); ok {
					schema.Required = append(schema.Required, name)
				}
			}
		}

		tool := anthropic.ToolParam{Name: t.Function.Name, InputSchema: schema}
		if t.Function.Description != "" {
			tool.Description = anthropic.String(t.Function.Description)
		}
		params.Tools = append(params.Tools, anthropic.ToolUnionParam{OfTool: &tool})
	}

	if len(params.Tools) > 0 {
		if strings.Trim(string(req.ToolChoice), `"`) == "none" {
			params.ToolChoice = anthropic.ToolChoiceUnionParam{OfNone: &anthropic.ToolChoiceNoneParam{}}
		} else {
			params.ToolChoice = anthropic.ToolChoiceUnionParam{OfAuto: &anthropic.ToolChoiceAutoParam{}}
		}
	}

	markCacheable(&params)
	return params, nil
}

// markCacheable asks Claude to cache the request's prefix: the system prompt, and everything up to the end of the
// last message. An agent's turns re-send the same conversation with one more exchange each time; Claude (unlike
// Azure OpenAI, which caches a repeated prefix on its own) caches only where it is asked to, and reads a cached prefix
// for a tenth of the price. Measured (2026-10-20): an operator turn re-sends about 8,000-10,000 tokens of contract,
// rules and stage request before any output, so a 15-turn stage paid for them 15 times. Nothing in the request
// changes: the marks only say where a cached prefix may end.
func markCacheable(params *anthropic.MessageNewParams) {
	if n := len(params.System); n > 0 {
		params.System[n-1].CacheControl = anthropic.NewCacheControlEphemeralParam()
	}
	if n := len(params.Messages); n > 0 {
		blocks := params.Messages[n-1].Content
		if k := len(blocks); k > 0 {
			if cc := blocks[k-1].GetCacheControl(); cc != nil {
				*cc = anthropic.NewCacheControlEphemeralParam()
			}
		}
	}
}

// contentBlocks reads an OpenAI message content - a string or a list of
// parts - as plain text and as Claude blocks (images only when allowed).
func contentBlocks(raw json.RawMessage, images bool) (string, []anthropic.ContentBlockParamUnion, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil, nil
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if text == "" {
			return "", nil, nil
		}
		return text, []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(text)}, nil
	}

	var parts []openAIPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", nil, errors.New("content is neither text nor a list of parts")
	}

	var texts []string
	var blocks []anthropic.ContentBlockParamUnion

	for _, p := range parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				texts = append(texts, p.Text)
				blocks = append(blocks, anthropic.NewTextBlock(p.Text))
			}
		case "image_url":
			if !images {
				return "", nil, errors.New("an image can only be in a user message")
			}
			block, err := imageBlock(p.ImageURL.URL)
			if err != nil {
				return "", nil, err
			}
			blocks = append(blocks, block)
		default:
			return "", nil, fmt.Errorf("content part %q is not supported", p.Type)
		}
	}

	return strings.Join(texts, "\n"), blocks, nil
}

// imageBlock turns an image_url (a data: URL or a web address) into a block.
func imageBlock(url string) (anthropic.ContentBlockParamUnion, error) {
	if rest, ok := strings.CutPrefix(url, "data:"); ok {
		meta, data, found := strings.Cut(rest, ",")
		mime, isBase64 := strings.CutSuffix(meta, ";base64")
		if !found || !isBase64 || data == "" {
			return anthropic.ContentBlockParamUnion{}, errors.New("an image data URL must be base64")
		}
		return anthropic.NewImageBlockBase64(mime, data), nil
	}

	if strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "http://") {
		return anthropic.NewImageBlock(anthropic.URLImageSourceParam{URL: url}), nil
	}

	return anthropic.ContentBlockParamUnion{}, errors.New("an image must be a data: URL or a web address")
}

// stopSequences reads OpenAI's stop, which is a string or a list.
func stopSequences(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}

	var one string
	if json.Unmarshal(raw, &one) == nil {
		if one == "" {
			return nil
		}
		return []string{one}
	}

	var many []string
	_ = json.Unmarshal(raw, &many)

	var out []string
	for _, s := range many {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}

	return out
}

// fromClaudeMessage writes Claude's answer as an OpenAI chat completion.
func fromClaudeMessage(msg *anthropic.Message) map[string]any {
	var text strings.Builder
	var calls []map[string]any

	for _, block := range msg.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "tool_use":
			args := string(block.Input)
			if args == "" {
				args = "{}"
			}
			calls = append(calls, map[string]any{
				"id":       block.ID,
				"type":     "function",
				"function": map[string]any{"name": block.Name, "arguments": args},
			})
		}
	}

	finish := "stop"
	switch msg.StopReason {
	case anthropic.StopReasonMaxTokens:
		finish = "length"
	case anthropic.StopReasonToolUse:
		finish = "tool_calls"
	case anthropic.StopReasonRefusal:
		finish = "content_filter"
	}

	prompt := msg.Usage.InputTokens + msg.Usage.CacheReadInputTokens + msg.Usage.CacheCreationInputTokens
	message := map[string]any{"role": "assistant", "content": text.String()}
	if len(calls) > 0 {
		message["tool_calls"] = calls
	}

	return map[string]any{
		"id":      msg.ID,
		"object":  "chat.completion",
		"model":   string(msg.Model),
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
		// Every prompt token counts in prompt_tokens, cached or not (a cached read is input that cost less), and the
		// cached part is said the way OpenAI says it, so each caller's accounting stays comparable.
		"usage": map[string]any{
			"prompt_tokens":     prompt,
			"completion_tokens": msg.Usage.OutputTokens,
			"total_tokens":      prompt + msg.Usage.OutputTokens,
			"prompt_tokens_details": map[string]any{
				"cached_tokens": msg.Usage.CacheReadInputTokens,
			},
		},
	}
}

// claudeErrorMessage is the API's own message for a failed call, without
// the request dump the SDK's Error() adds.
func claudeErrorMessage(e *anthropic.Error) string {
	if message := errorMessage([]byte(e.RawJSON())); message != "" {
		return message
	}

	return http.StatusText(e.StatusCode)
}

// errorMessage is the message of an OpenAI- or Claude-shaped error body
// ({"error": {"message": ...}}), or "".
func errorMessage(body []byte) string {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &parsed) != nil {
		return ""
	}

	return parsed.Error.Message
}

// errorBody is an OpenAI-shaped error answer.
func errorBody(message string) []byte {
	out, _ := json.Marshal(map[string]any{"error": map[string]any{"message": message}})
	return out
}
