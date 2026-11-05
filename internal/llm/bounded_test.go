package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A proxied call asks the provider for one whole answer within a bounded budget, whatever the desktop sent (C-20).
func TestTheProxyAsksForOneBoundedAnswer(t *testing.T) {
	var got map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	c := &Client{Endpoint: upstream.URL + "/models", Key: "k", Model: "big", HTTP: upstream.Client()}
	_, _, _, err := c.Chat(context.Background(), []byte(`{"model":"default","messages":[],"n":64,"stream":true,`+
		`"stream_options":{"include_usage":true},"logprobs":true,"top_logprobs":20,"max_tokens":10000000,"max_completion_tokens":4000}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"n", "stream", "stream_options", "logprobs", "top_logprobs"} {
		if _, ok := got[field]; ok {
			t.Errorf("%s reached the provider", field)
		}
	}
	if got["max_tokens"] != float64(maxAnswerTokens) || got["max_completion_tokens"] != float64(4000) {
		t.Errorf("budgets = %v / %v", got["max_tokens"], got["max_completion_tokens"])
	}
}

// An answer longer than the proxy passes on is an error, never a cut body handed back as if whole.
func TestACutAnswerIsAnError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", maxAnswerBytes+10)))
	}))
	defer upstream.Close()

	c := &Client{Endpoint: upstream.URL + "/models", Key: "k", Model: "big", HTTP: upstream.Client()}
	if _, _, _, err := c.Chat(context.Background(), []byte(`{"model":"default","messages":[]}`)); !errors.Is(err, ErrAnswerTooLong) {
		t.Fatalf("Chat = %v, want ErrAnswerTooLong", err)
	}
}
