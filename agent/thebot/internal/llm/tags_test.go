package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// fakeOpenAI answers every chat call and keeps each request's headers and raw body.
type fakeOpenAI struct {
	mu      sync.Mutex
	headers []http.Header
	bodies  [][]byte
}

func (f *fakeOpenAI) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.headers = append(f.headers, r.Header.Clone())
		f.bodies = append(f.bodies, body)
		f.mu.Unlock()
		if r.URL.Path == "/v1/embeddings" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"embedding": []float32{1, 2}}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "ok"}}},
			"usage":   map[string]any{"prompt_tokens": 3, "completion_tokens": 1},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOpenAISendsTheTagsAsHeaders(t *testing.T) {
	fake := &fakeOpenAI{}
	p := NewOpenAI("k", "")
	p.BaseURL = fake.server(t).URL

	ctx := NextTurn(WithTags(context.Background(), Tags{Task: "t_42", Job: "j9"}))
	if _, err := p.Chat(ctx, "m", []Message{{Role: "user", Content: "hi"}}, Options{Purpose: PurposeOperatorTurn}); err != nil {
		t.Fatal(err)
	}
	h := fake.headers[0]
	want := map[string]string{HeaderPurpose: "operator.turn", HeaderTask: "t_42", HeaderTurn: "j9:1", HeaderCaller: "soul"}
	for k, v := range want {
		if got := h.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if h.Get("Authorization") != "Bearer k" {
		t.Errorf("Authorization lost: %q", h.Get("Authorization"))
	}

	// No tags and no purpose: the call still goes, untagged, with no task or turn header.
	if _, err := p.Chat(context.Background(), "m", []Message{{Role: "user", Content: "hi"}}, Options{}); err != nil {
		t.Fatal(err)
	}
	h = fake.headers[1]
	if h.Get(HeaderPurpose) != PurposeUntagged || h.Get(HeaderCaller) != "soul" {
		t.Errorf("untagged headers = %v", h)
	}
	if _, ok := h[HeaderTask]; ok {
		t.Errorf("a task header was sent with no task: %v", h)
	}
	if _, ok := h[HeaderTurn]; ok {
		t.Errorf("a turn header was sent with no turn: %v", h)
	}
}

// TestTagsNeverChangeTheRequestBody pins the prompt prefix: the same messages sent with different tags and purposes
// give byte-identical request bodies, so the provider's prompt cache (and Ollama's loaded prefix) is never broken by a
// tag.
func TestTagsNeverChangeTheRequestBody(t *testing.T) {
	fake := &fakeOpenAI{}
	p := NewOpenAI("k", "")
	p.BaseURL = fake.server(t).URL
	msgs := []Message{{Role: "system", Content: "you are a worker"}, {Role: "user", Content: "do the thing"},
		{Role: "assistant", Content: "RUN: ls"}, {Role: "user", Content: "a.txt"}}

	a := NextTurn(WithTags(context.Background(), Tags{Task: "t_1", Job: "j1"}))
	b := NextTurn(NextTurn(WithTags(context.Background(), Tags{Task: "t_2", Job: "j2"})))
	if _, err := p.Chat(a, "m", msgs, Options{Purpose: PurposeOperatorTurn, Temperature: 0.2}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Chat(b, "m", msgs, Options{Purpose: PurposeReview, Temperature: 0.2}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Chat(context.Background(), "m", msgs, Options{Temperature: 0.2}); err != nil {
		t.Fatal(err)
	}
	if fake.headers[0].Get(HeaderTurn) == fake.headers[1].Get(HeaderTurn) {
		t.Fatal("the two calls were not tagged differently")
	}
	for i := 1; i < len(fake.bodies); i++ {
		if !bytes.Equal(fake.bodies[0], fake.bodies[i]) {
			t.Fatalf("request body %d differs from the first:\n%s\n%s", i, fake.bodies[0], fake.bodies[i])
		}
	}
	for _, tag := range []string{"t_1", "t_2", "j1:1", "j2:2", "operator.turn", "review", "soul"} {
		if bytes.Contains(fake.bodies[0], []byte(tag)) {
			t.Fatalf("the body carries the tag %q", tag)
		}
	}
}

func TestOllamaBodyIgnoresTags(t *testing.T) {
	var bodies [][]byte
	var headers []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, b)
		headers = append(headers, r.Header.Clone())
		_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{"content": "ok"}, "done": true})
	}))
	defer srv.Close()
	p := NewOllama(srv.URL)
	msgs := []Message{{Role: "user", Content: "hi"}}
	a := NextTurn(WithTags(context.Background(), Tags{Task: "t_1", Job: "j1"}))
	if _, err := p.Chat(a, "m", msgs, Options{Purpose: PurposeOperatorTurn, NumCtx: 8192}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Chat(context.Background(), "m", msgs, Options{Purpose: PurposeReview, NumCtx: 8192}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatalf("Ollama bodies differ:\n%s\n%s", bodies[0], bodies[1])
	}
	if !bytes.Contains(bodies[0], []byte(`"num_ctx":8192`)) {
		t.Fatalf("num_ctx is not 8192: %s", bodies[0])
	}
	if headers[0].Get(HeaderPurpose) != "" || headers[0].Get(HeaderTask) != "" {
		t.Fatalf("Ollama is sent the proxy's tags: %v", headers[0])
	}
}

func TestEmbeddingsAreTaggedEmbed(t *testing.T) {
	fake := &fakeOpenAI{}
	p := NewOpenAI("k", "")
	p.BaseURL = fake.server(t).URL
	if _, err := p.Embed(WithTags(context.Background(), Tags{Task: "t_1"}), "", "text"); err != nil {
		t.Fatal(err)
	}
	if h := fake.headers[0]; h.Get(HeaderPurpose) != PurposeEmbed || h.Get(HeaderTask) != "t_1" || h.Get(HeaderCaller) != "soul" {
		t.Fatalf("embedding headers = %v", h)
	}
}

func TestTagsShapes(t *testing.T) {
	if got := TagsFrom(WithTags(context.Background(), Tags{Task: "bad id; rm", Job: "j"})).Task; got != "" {
		t.Fatalf("a malformed task id was kept: %q", got)
	}
	if got := TagsFrom(WithTags(context.Background(), Tags{Task: "t_ok-1"})).Task; got != "t_ok-1" {
		t.Fatalf("a good task id was dropped: %q", got)
	}
	if got := TagsFrom(context.Background()); got != (Tags{}) {
		t.Fatalf("an untagged context has tags: %+v", got)
	}
	// No job: no turn.
	if got := TagsFrom(NextTurn(WithTags(context.Background(), Tags{Task: "t"}))).Turn; got != "" {
		t.Fatalf("a turn without a job: %q", got)
	}
	// One job's counter is shared by every context derived from it.
	job := WithTags(context.Background(), Tags{Job: "j7"})
	first, second := NextTurn(job), NextTurn(job)
	if TagsFrom(first).Turn != "j7:1" || TagsFrom(second).Turn != "j7:2" {
		t.Fatalf("turns = %q, %q", TagsFrom(first).Turn, TagsFrom(second).Turn)
	}
	if TagsFrom(NextTurn(first)).Turn != "j7:3" {
		t.Fatal("a turn derived from a turn did not continue the job's count")
	}
	if h := TagHeaders(context.Background(), "Bad Purpose!"); h[HeaderPurpose] != PurposeUntagged {
		t.Fatalf("a malformed purpose was sent: %v", h)
	}
}
