package backend

import (
	"context"
	"encoding/json"
	"errors"
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

// A stream that ends with no finish (the server died mid-answer) is an
// error, never a complete answer: "The total is 1" is not the total.
func TestACutStreamIsAnError(t *testing.T) {
	cases := map[string]struct {
		body string
		chat func(url string) (ChatResult, error)
	}{
		"openai": {"data: {\"choices\":[{\"delta\":{\"content\":\"The total is 1\"}}]}\n\n", func(url string) (ChatResult, error) {
			return (&OpenAI{BaseURL: url}).Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Text: "sum"}}, OnDelta: func(string) {}})
		}},
		"ollama": {`{"message":{"content":"The total is 1"},"done":false}` + "\n", func(url string) (ChatResult, error) {
			return NewOllama(url).Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: "user", Text: "sum"}}})
		}},
	}
	for name, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, c.body)
			w.(http.Flusher).Flush()
		}))
		res, err := c.chat(srv.URL)
		srv.Close()
		if err == nil || !errors.Is(err, io.ErrUnexpectedEOF) || res.Complete {
			t.Errorf("%s: a cut stream came back as an answer: err=%v text=%q complete=%v", name, err, res.Text, res.Complete)
		}
	}
}

// A finished answer says so.
func TestAFinishedAnswerIsComplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"12"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	res, err := (&OpenAI{BaseURL: srv.URL}).Chat(context.Background(), ChatRequest{OnDelta: func(string) {}})
	if err != nil || !res.Complete || res.Text != "12" {
		t.Fatalf("res %+v err %v", res, err)
	}
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[]}`)
	}))
	defer srv2.Close()
	if _, err := (&OpenAI{BaseURL: srv2.URL}).Chat(context.Background(), ChatRequest{}); err == nil {
		t.Fatal("an answer with no choices was accepted")
	}
}

// llama-server's own timings arrive with the answer, streamed or not: they
// say how much of the prompt its cache held and how fast it read and wrote.
func TestLlamaServerTimingsAreRead(t *testing.T) {
	timings := `"timings":{"cache_n":900,"prompt_n":100,"prompt_ms":2000.5,"predicted_n":20,"predicted_ms":4000,"draft_n":8,"draft_n_accepted":6}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		json.NewDecoder(r.Body).Decode(&got)
		if got["stream"] == true {
			fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"A"},"finish_reason":"stop"}]}`)
			fmt.Fprintln(w, `data: {"choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":20},`+timings+`}`)
			fmt.Fprintln(w, `data: [DONE]`)
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"A"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":20},`+timings+`}`)
	}))
	defer srv.Close()
	c := &OpenAI{BaseURL: srv.URL}
	want := Timings{CacheN: 900, PromptN: 100, PromptMs: 2000.5, PredictedN: 20, PredictedMs: 4000, DraftN: 8, DraftAccepted: 6}
	for _, stream := range []bool{false, true} {
		req := ChatRequest{Messages: []Message{{Role: "user", Text: "x"}}}
		if stream {
			req.OnDelta = func(string) {}
		}
		res, err := c.Chat(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if res.Timings == nil || *res.Timings != want {
			t.Fatalf("stream %v: timings %+v, want %+v", stream, res.Timings, want)
		}
	}
}
