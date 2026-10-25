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

// Ollama is an Ollama server. It is usually shared (Decurix desktops use it
// too), so two rules hold: every call uses the same NumCtx (a different
// window loads a second copy of the model, minutes on a small host), and
// Brainiac never sends it a request that is not real work.
type Ollama struct {
	URL       string
	NumCtx    int    // default 8192
	KeepAlive string // default "30m"
	NumThread int    // 0 lets Ollama decide
	HTTP      *http.Client
}

// NewOllama returns a client for the server at url.
func NewOllama(url string) *Ollama {
	return &Ollama{URL: strings.TrimRight(url, "/"), NumCtx: 8192, KeepAlive: "30m", HTTP: &http.Client{}}
}

type ollamaMessage struct {
	Role    string   `json:"role"`
	Content string   `json:"content"`
	Images  []string `json:"images,omitempty"`
}

type ollamaChunk struct {
	Model   string `json:"model"`
	Message struct {
		Content  string `json:"content"`
		Thinking string `json:"thinking"`
	} `json:"message"`
	Done            bool   `json:"done"`
	DoneReason      string `json:"done_reason"`
	PromptEvalCount int    `json:"prompt_eval_count"`
	EvalCount       int    `json:"eval_count"`
	Error           string `json:"error"`
}

// Chat runs one conversation turn. It always streams from Ollama (a proxy's
// idle timeout never cuts a long answer) and forwards deltas when asked.
func (o *Ollama) Chat(ctx context.Context, req ChatRequest) (ChatResult, error) {
	start := time.Now()
	msgs := make([]ollamaMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		om := ollamaMessage{Role: m.Role, Content: m.Text}
		var labels []string
		for i, img := range m.Images {
			om.Images = append(om.Images, base64.StdEncoding.EncodeToString(img.Data))
			if img.Label != "" {
				labels = append(labels, fmt.Sprintf("%d) %s", i+1, img.Label))
			}
		}
		if len(labels) > 0 {
			om.Content += "\n\n(The pictures, in order: " + strings.Join(labels, "; ") + ")"
		}
		msgs = append(msgs, om)
	}
	opts := map[string]any{"num_ctx": o.numCtx()}
	if req.Temperature != nil {
		opts["temperature"] = *req.Temperature
	}
	if req.MaxTokens > 0 {
		opts["num_predict"] = req.MaxTokens
	}
	if len(req.Stop) > 0 {
		opts["stop"] = req.Stop
	}
	if o.NumThread > 0 {
		opts["num_thread"] = o.NumThread
	}
	body := map[string]any{
		"model": req.Model, "messages": msgs, "stream": true, "options": opts,
		"keep_alive": o.keepAlive(), "think": req.Think,
	}
	if req.JSON {
		body["format"] = "json"
	}
	resp, err := o.post(ctx, "/api/chat", body)
	if err != nil {
		return ChatResult{}, err
	}
	defer resp.Body.Close()

	var out ChatResult
	var text strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var c ollamaChunk
		if err := json.Unmarshal(line, &c); err != nil {
			return out, fmt.Errorf("ollama: bad stream line: %w", err)
		}
		if c.Error != "" {
			return out, &HTTPError{Backend: "ollama", Status: http.StatusInternalServerError, Message: c.Error}
		}
		if c.Message.Content != "" {
			text.WriteString(c.Message.Content)
			if req.OnDelta != nil {
				req.OnDelta(c.Message.Content)
			}
		}
		if c.Message.Thinking != "" && req.OnThink != nil {
			req.OnThink(c.Message.Thinking)
		}
		if c.Done {
			out.PromptTokens, out.CompletionTokens = c.PromptEvalCount, c.EvalCount
			out.Truncated = c.DoneReason == "length"
			out.Model = c.Model
		}
	}
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("ollama: reading the answer: %w", err)
	}
	out.Text = text.String()
	out.Duration = time.Since(start)
	return out, nil
}

// Embed turns inputs into vectors with /api/embed.
func (o *Ollama) Embed(ctx context.Context, model string, inputs []string) ([][]float32, error) {
	resp, err := o.post(ctx, "/api/embed", map[string]any{"model": model, "input": inputs, "keep_alive": o.keepAlive()})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("ollama: embed answer: %w", err)
	}
	if len(out.Embeddings) != len(inputs) {
		return nil, fmt.Errorf("ollama: %d vectors for %d inputs", len(out.Embeddings), len(inputs))
	}
	return out.Embeddings, nil
}

// Unload asks Ollama to free a model now (keep_alive 0). Brainiac calls it
// only for models it loaded itself.
func (o *Ollama) Unload(ctx context.Context, model string) error {
	resp, err := o.post(ctx, "/api/generate", map[string]any{"model": model, "keep_alive": 0})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Loaded is what Ollama holds in memory now: model name to bytes.
func (o *Ollama) Loaded(ctx context.Context) (map[string]int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.URL+"/api/ps", nil)
	if err != nil {
		return nil, err
	}
	resp, err := o.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, readError("ollama", resp)
	}
	var ps struct {
		Models []struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ps); err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, m := range ps.Models {
		out[m.Name] = m.Size
	}
	return out, nil
}

// Pull downloads a model into Ollama, reporting progress lines.
func (o *Ollama) Pull(ctx context.Context, model string, progress func(status string, done, total int64)) error {
	resp, err := o.post(ctx, "/api/pull", map[string]any{"model": model, "stream": true})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var p struct {
			Status    string `json:"status"`
			Completed int64  `json:"completed"`
			Total     int64  `json:"total"`
			Error     string `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &p) != nil {
			continue
		}
		if p.Error != "" {
			return fmt.Errorf("ollama pull %s: %s", model, p.Error)
		}
		if progress != nil {
			progress(p.Status, p.Completed, p.Total)
		}
	}
	return sc.Err()
}

func (o *Ollama) post(ctx context.Context, path string, body any) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.URL+path, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, readError("ollama", resp)
	}
	return resp, nil
}

func (o *Ollama) client() *http.Client {
	if o.HTTP != nil {
		return o.HTTP
	}
	return http.DefaultClient
}

func (o *Ollama) numCtx() int {
	if o.NumCtx > 0 {
		return o.NumCtx
	}
	return 8192
}

func (o *Ollama) keepAlive() string {
	if o.KeepAlive != "" {
		return o.KeepAlive
	}
	return "30m"
}

func jsonErrorMessage(body []byte) string {
	var e struct {
		Error any `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil || e.Error == nil {
		return ""
	}
	switch v := e.Error.(type) {
	case string:
		return v
	case map[string]any:
		if m, ok := v["message"].(string); ok {
			return m
		}
	}
	return ""
}
