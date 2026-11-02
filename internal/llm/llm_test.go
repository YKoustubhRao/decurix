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

func TestAnOpenAICompatibleClientUsesBearerAndTheV1Route(t *testing.T) {
	var gotAuth, gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	// A base address without its /v1 (a Brainiac box: http://host:8484) gets it; one with it is left alone.
	c := NewOpenAI(upstream.URL, "sk-x", "", "")
	c.HTTP = upstream.Client()
	if c.Endpoint != upstream.URL+"/v1" || c.Model != DefaultModel || !c.Bearer {
		t.Fatalf("client = %+v", c)
	}
	if again := NewOpenAI(upstream.URL+"/v1", "k", "m", "f"); again.Endpoint != upstream.URL+"/v1" {
		t.Fatalf("a /v1 endpoint was changed to %q", again.Endpoint)
	}
	if NewOpenAI("", "k", "", "").Endpoint != DefaultOpenAIEndpoint {
		t.Fatal("no endpoint did not mean OpenAI itself")
	}

	status, _, _, err := c.Chat(context.Background(), []byte(`{"model":"default","messages":[]}`))
	if err != nil || status != 200 {
		t.Fatalf("Chat = %d %v", status, err)
	}
	if gotAuth != "Bearer sk-x" || gotPath != "/v1/chat/completions" {
		t.Fatalf("upstream saw auth=%q path=%q", gotAuth, gotPath)
	}
}

func TestAnOpenAIEndpointMayBePlainHTTPOnlyOnTheOwnNetwork(t *testing.T) {
	hub, _ := NewHub(nil, nil)
	for _, endpoint := range []string{"", "https://openrouter.ai/api/v1", "http://localhost:8484", "http://192.168.1.20:8484", "http://host.docker.internal:8484"} {
		if _, err := hub.Complete(ProviderOpenAI, Settings{Endpoint: endpoint, Key: "k"}); err != nil {
			t.Errorf("%q refused: %v", endpoint, err)
		}
	}
	for _, endpoint := range []string{"http://api.openai.com/v1", "http://8.8.8.8:8484", "ftp://x"} {
		if _, err := hub.Complete(ProviderOpenAI, Settings{Endpoint: endpoint, Key: "k"}); err == nil {
			t.Errorf("%q accepted: the key would go out in the clear", endpoint)
		}
	}
	s, _ := hub.Complete(ProviderOpenAI, Settings{Key: "k"})
	if s.Endpoint != DefaultOpenAIEndpoint {
		t.Fatalf("an empty endpoint became %q", s.Endpoint)
	}
	if err := hub.Save(ProviderOpenAI, Settings{Endpoint: "http://localhost:8484", Key: "none", Model: "qwen"}); err != nil {
		t.Fatal(err)
	}
	if st := hub.Status()[ProviderOpenAI]; !st.Configured || st.Endpoint != "http://localhost:8484/v1" || st.Model != "qwen" || !hub.AnyConfigured() {
		t.Fatalf("status = %+v", st)
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
