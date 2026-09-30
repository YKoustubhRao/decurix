package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatAddsTheKeyAndResolvesTheModel(t *testing.T) {
	var gotKey, gotModel, gotPath string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("api-key")
		gotPath = r.URL.Path

		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		gotModel, _ = body["model"].(string)

		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	c := &Client{Endpoint: upstream.URL + "/models", Key: "secret", Model: "big", FastModel: "small", HTTP: upstream.Client()}

	status, body, _, err := c.Chat(context.Background(), []byte(`{"model":"fast","messages":[]}`))
	if err != nil || status != 200 || !strings.Contains(string(body), "ok") {
		t.Fatalf("Chat = %d %s %v", status, body, err)
	}

	if gotKey != "secret" || gotModel != "small" || gotPath != "/models/chat/completions" {
		t.Fatalf("upstream saw key=%q model=%q path=%q", gotKey, gotModel, gotPath)
	}
}

func TestChatRefusesAModelItDoesNotOffer(t *testing.T) {
	c := &Client{Endpoint: "http://unused", Key: "k", Model: "big", FastModel: "small"}

	if _, _, _, err := c.Chat(context.Background(), []byte(`{"model":"something-expensive"}`)); err == nil {
		t.Fatal("an unoffered model was forwarded")
	}
}

func TestUnconfiguredClientSaysSo(t *testing.T) {
	c := &Client{Model: DefaultModel}

	if c.Configured() {
		t.Fatal("a client with no key reports configured")
	}

	if _, _, _, err := c.Chat(context.Background(), []byte(`{}`)); err != ErrNotConfigured {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}

func TestARateLimitSaysWhenToTryAgain(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "37")
		w.Header().Set("X-Internal", "not passed on")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"rate_limit_exceeded"}}`))
	}))
	defer upstream.Close()

	c := &Client{Endpoint: upstream.URL, Key: "k", Model: "big", FastModel: "small", HTTP: upstream.Client()}

	status, _, retry, err := c.Chat(context.Background(), []byte(`{"messages":[]}`))
	if err != nil || status != http.StatusTooManyRequests {
		t.Fatalf("Chat = %d, %v", status, err)
	}
	if retry.Get("Retry-After") != "37" || retry.Get("X-Internal") != "" {
		t.Fatalf("retry headers = %v", retry)
	}
}
