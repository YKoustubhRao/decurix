// Package llm gives the desktops a hosted model without giving them its key.
//
// A desktop's agent (the "azure" harness, and the grounder/judge of every
// harness) POSTs an OpenAI-shaped chat request to the control plane's
// /api/llm/chat, and Client.Chat forwards it to an Azure AI Foundry endpoint
// with the api-key header added here. The key lives only in the control
// plane's own environment: an agent that browses the web and runs shell
// commands can be talked into printing anything it can read, so the key is
// never baked into a container the way OLLAMA_URL is.
//
// Depends on nothing internal.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	// DefaultModel is the deployment used when AZURE_MODEL is unset: the
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

// ErrNotConfigured is a chat asked of a control plane with no AZURE_KEY.
var ErrNotConfigured = errors.New("llm: no hosted model is configured (AZURE_ENDPOINT / AZURE_KEY)")

// Client forwards chat requests to one Azure AI endpoint.
type Client struct {
	Endpoint  string
	Key       string
	Model     string
	FastModel string
	HTTP      *http.Client
}

// FromEnv builds a Client from AZURE_ENDPOINT, AZURE_KEY, AZURE_MODEL and
// AZURE_FAST_MODEL. The Client is returned even when unconfigured, so the
// status route can say so; Configured tells them apart.
func FromEnv() *Client {
	c := &Client{
		Endpoint:  strings.TrimRight(strings.TrimSpace(os.Getenv("AZURE_ENDPOINT")), "/"),
		Key:       strings.TrimSpace(os.Getenv("AZURE_KEY")),
		Model:     strings.TrimSpace(os.Getenv("AZURE_MODEL")),
		FastModel: strings.TrimSpace(os.Getenv("AZURE_FAST_MODEL")),
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

// Resolve maps what a caller asked for to a deployment: "" or "default" is
// Model, "fast" is FastModel, and the two configured names stand for
// themselves. Anything else is refused rather than forwarded, so a desktop
// cannot spend on whatever else happens to be deployed on the resource.
func (c *Client) Resolve(asked string) (string, error) {
	switch strings.TrimSpace(asked) {
	case "", "default", c.Model:
		return c.Model, nil
	case "fast", c.FastModel:
		return c.FastModel, nil
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
// resolved (Resolve), and returns the upstream status, body and the headers
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

	model, err := c.Resolve(asked)
	if err != nil {
		return 0, nil, nil, err
	}

	req["model"] = model

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

	retry := http.Header{}
	for _, name := range RetryHeaders {
		if v := resp.Header.Get(name); v != "" {
			retry.Set(name, v)
		}
	}

	return resp.StatusCode, data, retry, nil
}
