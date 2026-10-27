package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var png1x1 = []byte("\x89PNG\r\n\x1a\n0000")

func TestOllamaStreamsAndKeepsTheWindowFixed(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			http.NotFound(w, r)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		fmt.Fprintln(w, `{"message":{"content":"Hel"},"done":false}`)
		fmt.Fprintln(w, `{"message":{"content":"lo"},"done":false}`)
		fmt.Fprintln(w, `{"model":"m","message":{"content":""},"done":true,"done_reason":"length","prompt_eval_count":12,"eval_count":2}`)
	}))
	defer srv.Close()
	o := NewOllama(srv.URL)
	var deltas []string
	res, err := o.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{
		{Role: "user", Text: "hi", Images: []Image{{Data: png1x1, Label: "a.png"}}}}, OnDelta: func(s string) { deltas = append(deltas, s) }})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "Hello" || !res.Truncated || res.PromptTokens != 12 || len(deltas) != 2 {
		t.Fatalf("result %+v deltas %v", res, deltas)
	}
	opts := got["options"].(map[string]any)
	if opts["num_ctx"].(float64) != 8192 {
		t.Fatalf("num_ctx = %v", opts["num_ctx"])
	}
	if _, set := opts["temperature"]; set {
		t.Fatal("temperature sent though not asked")
	}
	msg := got["messages"].([]any)[0].(map[string]any)
	if len(msg["images"].([]any)) != 1 || !strings.Contains(msg["content"].(string), "a.png") {
		t.Fatalf("image or label missing: %v", msg)
	}
}

func TestOllamaErrorIsTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"error":"model is loading"}`)
	}))
	defer srv.Close()
	_, err := NewOllama(srv.URL).Chat(context.Background(), ChatRequest{Model: "m"})
	if !Transient(err) || !strings.Contains(err.Error(), "model is loading") {
		t.Fatalf("err = %v", err)
	}
}

func TestOpenAIStreamsDeltasAndUsage(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("api-key") != "k" || r.URL.Query().Get("api-version") != "v1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"A"}}]}`)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"B"},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w, `data: {"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":3}}}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer srv.Close()
	c := &OpenAI{BaseURL: srv.URL, ChatPath: "/models/chat/completions", Query: "api-version=v1", Key: "k", KeyHeader: "api-key", Reasoning: true}
	temp := 0.2
	var deltas strings.Builder
	res, err := c.Chat(context.Background(), ChatRequest{Model: "gpt-5.4", MaxTokens: 100, Temperature: &temp,
		Messages: []Message{{Role: "user", Text: "x", Images: []Image{{Data: png1x1}}}}, OnDelta: func(s string) { deltas.WriteString(s) }})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "AB" || deltas.String() != "AB" || res.CachedTokens != 3 || res.CompletionTokens != 2 {
		t.Fatalf("res %+v", res)
	}
	if _, ok := got["temperature"]; ok {
		t.Fatal("a reasoning model was sent temperature")
	}
	if got["max_completion_tokens"].(float64) != 100 {
		t.Fatalf("max_completion_tokens = %v", got["max_completion_tokens"])
	}
	parts := got["messages"].([]any)[0].(map[string]any)["content"].([]any)
	img := parts[1].(map[string]any)["image_url"].(map[string]any)["url"].(string)
	if !strings.HasPrefix(img, "data:image/png;base64,") {
		t.Fatalf("image url %q", img)
	}
}

func TestOpenAIWithoutStreamingStillDelivers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		json.NewDecoder(r.Body).Decode(&got)
		if got["stream"] == true || got["model"] != "default" {
			t.Errorf("stream %v model %v", got["stream"], got["model"])
		}
		io.WriteString(w, `{"model":"gpt-5.4","choices":[{"message":{"content":"whole"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()
	c := &OpenAI{BaseURL: srv.URL, Model: "default", NoStream: true}
	var got string
	res, err := c.Chat(context.Background(), ChatRequest{Model: "brainiac", OnDelta: func(s string) { got += s }})
	if err != nil || res.Text != "whole" || got != "whole" || res.Model != "gpt-5.4" {
		t.Fatalf("res %+v err %v delta %q", res, err, got)
	}
}

func TestOpenAIEmbedKeepsOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"index":1,"embedding":[2]},{"index":0,"embedding":[1]}]}`)
	}))
	defer srv.Close()
	v, err := (&OpenAI{BaseURL: srv.URL}).Embed(context.Background(), "e", []string{"a", "b"})
	if err != nil || v[0][0] != 1 || v[1][0] != 2 {
		t.Fatalf("v %v err %v", v, err)
	}
}

func TestImageMime(t *testing.T) {
	if ImageMime(png1x1) != "image/png" || ImageMime([]byte{0xFF, 0xD8, 0xFF, 0}) != "image/jpeg" || ImageMime([]byte("hello")) != "" {
		t.Fatal("mime sniffing")
	}
}
