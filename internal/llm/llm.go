// Package llm gives the desktops a hosted model without giving them its key.
//
// A desktop's agent (TheBot with a hosted brain, and the grounder/judge of
// every harness) POSTs an OpenAI-shaped chat request to the control plane's
// /api/llm/<provider>/..., and the Hub forwards it: to an Azure AI Foundry
// endpoint (Client, the api-key header added here) or to Claude (Claude,
// translated to the Messages API). The credentials are typed into the
// dashboard by a person and kept sealed in SQLite (Store) - never read from
// .env, never baked into a container: an agent that browses the web and runs
// shell commands can be talked into printing anything it can read.
//
// Depends on sqlitedb and sealed only.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// DefaultModel is the deployment used when none is given: the
	// strongest GPT deployment on the endpoint this was built against that
	// reads images and calls tools (checked 2026-09-28).
	DefaultModel = "gpt-5.4"
	// DefaultFastModel is the cheaper deployment asked for with "fast".
	DefaultFastModel = "gpt-5.4-mini"

	apiVersion = "2024-05-01-preview"

	// MaxRequestBytes bounds one request: a few screenshots and a scanned
	// page as base64 fit easily; a runaway loop resending a PDF does not.
	MaxRequestBytes = 24 << 20

	requestTimeout = 4 * time.Minute
)

// ErrNotConfigured is a chat asked of a provider nobody has set up yet.
var ErrNotConfigured = errors.New("llm: this hosted model has not been set up (dashboard: New desktop > Model)")

// Client forwards chat requests to one Azure AI endpoint.
type Client struct {
	Endpoint  string
	Key       string
	Model     string
	FastModel string
	HTTP      *http.Client
}

// NewAzure builds a Client for one Azure AI endpoint; empty model names take
// DefaultModel / DefaultFastModel.
func NewAzure(endpoint, key, model, fastModel string) *Client {
	c := &Client{
		Endpoint:  strings.TrimRight(strings.TrimSpace(endpoint), "/"),
		Key:       strings.TrimSpace(key),
		Model:     strings.TrimSpace(model),
		FastModel: strings.TrimSpace(fastModel),
		HTTP:      &http.Client{Timeout: requestTimeout},
	}

	if c.Model == "" {
		c.Model = DefaultModel
	}

	if c.FastModel == "" {
		c.FastModel = DefaultFastModel
	}

	return c
}

// Configured reports whether chats can be forwarded at all.
func (c *Client) Configured() bool {
	return c != nil && c.Endpoint != "" && c.Key != ""
}

// resolve maps what a caller asked a provider for to one of its models: ""
// or "default" is model, "fast" is fast, and the two configured names stand
// for themselves. Anything else is refused rather than forwarded, so a
// desktop cannot spend on whatever else happens to be deployed on the
// resource.
func resolve(asked, model, fast string) (string, error) {
	switch strings.TrimSpace(asked) {
	case "", "default", model:
		return model, nil
	case "fast", fast:
		return fast, nil
	}

	return "", fmt.Errorf("llm: model %q is not one this control plane offers", asked)
}

// chatURL is the endpoint's chat-completions address. An endpoint given as
// the project's ".../models" inference root and one given as a bare resource
// both work.
func (c *Client) chatURL() string {
	base := c.Endpoint
	if !strings.HasSuffix(base, "/models") {
		base += "/models"
	}

	return base + "/chat/completions?api-version=" + apiVersion
}

// RetryHeaders are the upstream headers that say when to try again after a
// rate limit (429): passed through to the desktop, which waits that long.
var RetryHeaders = []string{"Retry-After", "Retry-After-Ms", "X-Ratelimit-Reset-Tokens", "X-Ratelimit-Reset-Requests"}

// Chat forwards one OpenAI-shaped chat request body, with its "model" field
// resolved (resolve), and returns the upstream status, body and the headers
// in RetryHeaders it sent, unchanged.
func (c *Client) Chat(ctx context.Context, body []byte) (int, []byte, http.Header, error) {
	if !c.Configured() {
		return 0, nil, nil, ErrNotConfigured
	}

	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return 0, nil, nil, fmt.Errorf("llm: request is not a JSON object: %w", err)
	}

	asked, _ := req["model"].(string)

	model, err := resolve(asked, c.Model, c.FastModel)
	if err != nil {
		return 0, nil, nil, err
	}

	req["model"] = model

	// Reasoning deployments reject the legacy token budget and sampling knobs.
	// Normalize at the provider boundary for every harness using this proxy.
	if isReasoningModel(model) {
		if limit, ok := req["max_tokens"]; ok {
			if _, explicit := req["max_completion_tokens"]; !explicit {
				req["max_completion_tokens"] = limit
			}
			delete(req, "max_tokens")
		}
		for _, field := range []string{"temperature", "top_p", "presence_penalty", "frequency_penalty", "logprobs", "top_logprobs"} {
			delete(req, field)
		}
	}

	out, err := json.Marshal(req)
	if err != nil {
		return 0, nil, nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatURL(), bytes.NewReader(out))
	if err != nil {
		return 0, nil, nil, err
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("api-key", c.Key)

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("llm: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("llm: reading the answer: %w", err)
	}

	return resp.StatusCode, data, copyHeaders(resp.Header, RetryHeaders), nil
}

// copyHeaders is the headers of from named in names that are set.
func copyHeaders(from http.Header, names []string) http.Header {
	out := http.Header{}
	for _, name := range names {
		if v := from.Get(name); v != "" {
			out.Set(name, v)
		}
	}

	return out
}

// isReasoningModel recognizes the supported reasoning deployment families.
func isReasoningModel(model string) bool {
	name := strings.ToLower(model)
	return strings.HasPrefix(name, "gpt-5") || strings.HasPrefix(name, "gpt-6") ||
		name == "o1" || strings.HasPrefix(name, "o1-") ||
		name == "o3" || strings.HasPrefix(name, "o3-") ||
		name == "o4" || strings.HasPrefix(name, "o4-")
}
