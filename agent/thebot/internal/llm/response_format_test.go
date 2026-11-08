package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The Python agent's calls through soul ask for JSON (response_format); soul's own calls never do, and their request
// bodies must stay byte-identical to before (the provider's prompt cache).
func TestResponseFormatIsSentOnlyWhenACallNamesOne(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		bodies = append(bodies, body)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{}"}}]}`))
	}))
	defer srv.Close()
	o := &OpenAIProvider{APIKey: "k", Client: srv.Client(), BaseURL: srv.URL}
	msgs := []Message{{Role: "user", Content: "x"}}
	if _, err := o.Chat(context.Background(), "m", msgs, Options{Purpose: PurposeReview}); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Chat(context.Background(), "m", msgs, Options{Purpose: PurposeReview, ResponseFormat: `{"type":"json_object"}`}); err != nil {
		t.Fatal(err)
	}
	if _, present := bodies[0]["response_format"]; present {
		t.Fatalf("a call that names no format sent one: %v", bodies[0])
	}
	if rf, _ := bodies[1]["response_format"].(map[string]any); rf["type"] != "json_object" {
		t.Fatalf("response_format: %v", bodies[1]["response_format"])
	}
}

func TestOllamaTakesAResponseFormatAsItsFormat(t *testing.T) {
	for in, want := range map[string]string{
		"":                       "",
		`{"type":"text"}`:        "",
		`not json`:               "",
		`{"type":"json_object"}`: `"json"`,
		`{"type":"json_schema","json_schema":{"name":"x","schema":{"type":"object"}}}`: `{"type":"object"}`,
		`{"type":"json_schema"}`: `"json"`,
	} {
		if got := string(ollamaFormat(in)); got != want {
			t.Errorf("ollamaFormat(%s) = %s, want %s", in, got, want)
		}
	}
}
