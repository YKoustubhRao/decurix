package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// effortSent is the reasoning_effort a Chat call put on the wire.
func effortSent(t *testing.T, o *OpenAIProvider) (string, bool) {
	t.Helper()
	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&sent)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()
	o.BaseURL = srv.URL
	if _, err := o.Chat(context.Background(), "gpt-5.4", []Message{{Role: "user", Content: "hi"}}, Options{}); err != nil {
		t.Fatal(err)
	}
	effort, present := sent["reasoning_effort"].(string)
	return effort, present
}

func TestReasoningEffortIsSentOnlyWhenAPersonSetIt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if _, present := effortSent(t, NewOpenAI("k", "")); present {
		t.Error("an effort was sent with none set: the model's default must stand")
	}
	if err := os.MkdirAll(filepath.Join(home, ".decurix"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(home, ".decurix", "reasoning-effort")
	if err := os.WriteFile(file, []byte("high\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if effort, _ := effortSent(t, NewOpenAI("k", "")); effort != "high" {
		t.Errorf("effort %q", effort)
	}
	if err := os.WriteFile(file, []byte("as hard as you can"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, present := effortSent(t, NewOpenAI("k", "")); present {
		t.Error("a value that is no effort was sent")
	}
}
