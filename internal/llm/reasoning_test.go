package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestChatReasoningCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, model, request string
		wantLimit            float64
		legacy               bool
	}{
		{"main", "gpt-5.4", `{"model":"default","max_tokens":8000,"temperature":0.2,"top_p":0.9,"presence_penalty":1,"frequency_penalty":1,"logprobs":true,"top_logprobs":2,"messages":[],"tools":[]}`, 8000, false},
		{"explicit budget", "gpt-5.4-mini", `{"max_tokens":8000,"max_completion_tokens":16000,"messages":[]}`, 16000, false},
		{"o-series", "o3", `{"max_tokens":8000,"messages":[]}`, 8000, false},
		{"legacy", "gpt-4o", `{"max_tokens":8000,"temperature":0.2,"messages":[]}`, 8000, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req map[string]any
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				key := "max_completion_tokens"
				if tc.legacy {
					key = "max_tokens"
					if req["temperature"] != 0.2 {
						t.Error("legacy sampling changed")
					}
				} else {
					for _, field := range []string{"max_tokens", "temperature", "top_p", "presence_penalty", "frequency_penalty", "logprobs", "top_logprobs"} {
						if _, ok := req[field]; ok {
							t.Errorf("unsupported %s forwarded", field)
						}
					}
				}
				if req[key] != tc.wantLimit {
					t.Errorf("budget = %v, want %v", req[key], tc.wantLimit)
				}
				if req["model"] != tc.model {
					t.Error("wrong resolved deployment")
				}
				if _, ok := req["messages"]; !ok {
					t.Error("messages dropped")
				}
				w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
			}))
			defer upstream.Close()
			c := NewAzure(upstream.URL, "secret", tc.model, "")
			c.HTTP = upstream.Client()
			status, _, _, err := c.Chat(context.Background(), []byte(tc.request))
			if err != nil || status != 200 {
				t.Fatalf("Chat=%d %v", status, err)
			}
		})
	}
}
