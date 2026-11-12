package backend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// OpenAI is any server speaking OpenAI's chat-completions API: Brainiac's own
// llama-server, vLLM, an Azure AI endpoint, or Decurix's control-plane proxy
// (which holds the Claude and Azure keys, as it does for TheBot).
type OpenAI struct {
	Name string // for errors: "llama-server", "azure", "decurix"
	// BaseURL is everything before ChatPath ("http://127.0.0.1:9931").
	BaseURL string
	// ChatPath defaults to "/v1/chat/completions"; EmbedPath to "/v1/embeddings".
	ChatPath  string
	EmbedPath string
	// Query is appended to every URL ("api-version=2024-05-01-preview").
	Query string
	// Key is sent as "Authorization: Bearer <key>", or in KeyHeader ("api-key").
	Key       string
	KeyHeader string
	// Model replaces the request's model name when set (the proxy takes
	// "default"; a single-model llama-server ignores it).
	Model string
	// NoStream asks for the whole answer at once, for servers that cannot
	// stream (the Decurix proxy buffers); deltas are then sent in one piece.
	NoStream bool
	// Reasoning marks a hosted reasoning model (gpt-5.x): it takes
	// max_completion_tokens and refuses sampling knobs.
	Reasoning bool
	// TemplateThinking sends the request's Think as the chat template's
	// enable_thinking (llama-server with a hybrid-thinking model such as Qwen3.5).
	TemplateThinking bool
	HTTP             *http.Client
}

type oaPart struct {
	Type     string      `json:"type"`
	Text     string      `json:"text,omitempty"`
	ImageURL *oaImageURL `json:"image_url,omitempty"`
}

type oaImageURL struct {
	URL string `json:"url"`
}

type oaMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

func oaMessages(msgs []Message) []oaMessage {
	out := make([]oaMessage, 0, len(msgs))
	for _, m := range msgs {
		if len(m.Images) == 0 {
			out = append(out, oaMessage{Role: m.Role, Content: m.Text})
			continue
		}
		var parts []oaPart
		if m.Text != "" {
			parts = append(parts, oaPart{Type: "text", Text: m.Text})
		}
		for i, img := range m.Images {
			if img.Label != "" {
				parts = append(parts, oaPart{Type: "text", Text: fmt.Sprintf("Picture %d: %s", i+1, img.Label)})
			}
			mime := img.Mime
			if mime == "" {
				mime = ImageMime(img.Data)
			}
			parts = append(parts, oaPart{Type: "image_url", ImageURL: &oaImageURL{
				URL: "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(img.Data)}})
		}
		out = append(out, oaMessage{Role: m.Role, Content: parts})
	}
	return out
}

type oaUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

type oaResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *oaUsage `json:"usage"`
	// Timings is llama-server's own field (on the last chunk of a stream).
	Timings *Timings `json:"timings"`
}

// Chat runs one conversation turn.
func (c *OpenAI) Chat(ctx context.Context, req ChatRequest) (ChatResult, error) {
	start := time.Now()
	model := req.Model
	if c.Model != "" {
		model = c.Model
	}
	body := map[string]any{"model": model, "messages": oaMessages(req.Messages)}
	stream := req.OnDelta != nil && !c.NoStream
	if stream {
		body["stream"] = true
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	if req.MaxTokens > 0 {
		if c.Reasoning {
			body["max_completion_tokens"] = req.MaxTokens
		} else {
			body["max_tokens"] = req.MaxTokens
		}
	}
	if req.Temperature != nil && !c.Reasoning {
		body["temperature"] = *req.Temperature
	}
	if len(req.Stop) > 0 {
		body["stop"] = req.Stop
	}
	if req.JSON {
		body["response_format"] = map[string]any{"type": "json_object"}
	}
	if c.TemplateThinking {
		body["chat_template_kwargs"] = map[string]any{"enable_thinking": req.Think}
	}
	resp, err := c.post(ctx, c.path(c.ChatPath, "/v1/chat/completions"), body)
	if err != nil {
		return ChatResult{}, err
	}
	defer resp.Body.Close()

	var out ChatResult
	if !stream {
		var r oaResponse
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			return out, fmt.Errorf("%s: bad answer: %w", c.name(), err)
		}
		if len(r.Choices) == 0 {
			return out, fmt.Errorf("%s: the answer has no choices", c.name())
		}
		out.Text = r.Choices[0].Message.Content
		out.Truncated = r.Choices[0].FinishReason == "length"
		out.Complete = true // a whole JSON document came back
		c.usage(&out, r.Usage)
		out.Model = r.Model
		out.Timings = r.Timings
		if req.OnDelta != nil && out.Text != "" {
			req.OnDelta(out.Text)
		}
		out.Duration = time.Since(start)
		return out, nil
	}

	var text strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	finished := false // a finish_reason or [DONE]: the server says the answer ended
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			finished = true
			break
		}
		var r oaResponse
		if err := json.Unmarshal([]byte(data), &r); err != nil {
			if m := jsonErrorMessage([]byte(data)); m != "" {
				return out, &HTTPError{Backend: c.name(), Status: http.StatusInternalServerError, Message: m}
			}
			continue
		}
		if r.Model != "" {
			out.Model = r.Model
		}
		for _, ch := range r.Choices {
			if d := ch.Delta.Content; d != "" {
				text.WriteString(d)
				req.OnDelta(d)
			}
			if t := ch.Delta.ReasoningContent; t != "" && req.OnThink != nil {
				req.OnThink(t)
			}
			if ch.FinishReason != "" {
				finished = true
			}
			if ch.FinishReason == "length" {
				out.Truncated = true
			}
		}
		c.usage(&out, r.Usage)
		if r.Timings != nil {
			out.Timings = r.Timings
		}
	}
	out.Text = text.String()
	out.Duration = time.Since(start)
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("%s: reading the answer: %w", c.name(), err)
	}
	if !finished {
		return out, fmt.Errorf("%s: %w", c.name(), ErrCutShort)
	}
	out.Complete = true
	return out, nil
}

func (c *OpenAI) usage(out *ChatResult, u *oaUsage) {
	if u == nil {
		return
	}
	out.PromptTokens, out.CompletionTokens = u.PromptTokens, u.CompletionTokens
	if u.PromptTokensDetails != nil {
		out.CachedTokens = u.PromptTokensDetails.CachedTokens
	}
}

// Embed turns inputs into vectors with /v1/embeddings.
func (c *OpenAI) Embed(ctx context.Context, model string, inputs []string) ([][]float32, error) {
	if c.Model != "" {
		model = c.Model
	}
	resp, err := c.post(ctx, c.path(c.EmbedPath, "/v1/embeddings"), map[string]any{"model": model, "input": inputs})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("%s: embed answer: %w", c.name(), err)
	}
	vecs := make([][]float32, len(inputs))
	for _, d := range out.Data {
		if d.Index >= 0 && d.Index < len(vecs) {
			vecs[d.Index] = d.Embedding
		}
	}
	for i, v := range vecs {
		if v == nil {
			return nil, fmt.Errorf("%s: no vector for input %d", c.name(), i)
		}
	}
	return vecs, nil
}

// SlotAction asks llama-server to save or restore one slot's prompt state
// ("save" | "restore") to or from filename in its --slot-save-path.
func (c *OpenAI) SlotAction(ctx context.Context, id int, action, filename string) error {
	resp, err := c.post(ctx, fmt.Sprintf("/slots/%d?action=%s", id, action), map[string]any{"filename": filename})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Healthy reports whether the server answers its health or models route.
func (c *OpenAI) Healthy(ctx context.Context) bool {
	for _, p := range []string{"/health", "/v1/models"} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(p), nil)
		if err != nil {
			return false
		}
		c.auth(req)
		resp, err := c.client().Do(req)
		if err != nil {
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return true
		}
	}
	return false
}

func (c *OpenAI) path(set, fallback string) string {
	if set != "" {
		return set
	}
	return fallback
}

func (c *OpenAI) url(path string) string {
	u := strings.TrimRight(c.BaseURL, "/") + path
	if c.Query != "" {
		sep := "?"
		if strings.Contains(u, "?") {
			sep = "&"
		}
		u += sep + c.Query
	}
	return u
}

func (c *OpenAI) auth(req *http.Request) {
	if c.Key == "" {
		return
	}
	if c.KeyHeader != "" && !strings.EqualFold(c.KeyHeader, "authorization") {
		req.Header.Set(c.KeyHeader, c.Key)
		return
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
}

func (c *OpenAI) post(ctx context.Context, path string, body any) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(path), bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.auth(req)
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", c.name(), err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, readError(c.name(), resp)
	}
	return resp, nil
}

func (c *OpenAI) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *OpenAI) name() string {
	if c.Name != "" {
		return c.Name
	}
	return "openai"
}
