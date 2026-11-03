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

	"github.com/YKoustubhRao/decurix/internal/sealed"
	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// fakeMessagesAPI answers /v1/messages with reply and records what it was sent.
func fakeMessagesAPI(t *testing.T, status int, reply string, header http.Header) (*httptest.Server, *map[string]any, *http.Header) {
	t.Helper()

	var got map[string]any
	var gotHeader http.Header

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %s", r.URL.Path)
		}
		gotHeader = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)

		for k, vs := range header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)

	return srv, &got, &gotHeader
}

func TestClaudeTranslatesAnOpenAIChatBothWays(t *testing.T) {
	srv, got, header := fakeMessagesAPI(t, 200, `{
		"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5",
		"content":[{"type":"text","text":"Reading it."},{"type":"tool_use","id":"tu_2","name":"read_document","input":{"path":"b.pdf"}}],
		"stop_reason":"tool_use","usage":{"input_tokens":120,"output_tokens":30}}`, nil)

	c := NewClaude("sk-test", "", "")
	c.BaseURL = srv.URL

	body := `{"model":"default","temperature":0.2,"max_completion_tokens":900,"stop":"END",
	  "messages":[
	    {"role":"system","content":"You are TheBot."},
	    {"role":"user","content":[{"type":"text","text":"What is in this?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]},
	    {"role":"assistant","content":null,"tool_calls":[{"id":"tu_1","type":"function","function":{"name":"read_document","arguments":"{\"path\":\"a.pdf\"}"}}]},
	    {"role":"tool","tool_call_id":"tu_1","content":"Invoice INV-7"},
	    {"role":"user","content":"And the next one?"}
	  ],
	  "tools":[{"type":"function","function":{"name":"read_document","description":"Read a file","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}}],
	  "tool_choice":"required"}`

	status, answer, _, err := c.Chat(context.Background(), []byte(body))
	if err != nil || status != 200 {
		t.Fatalf("Chat = %d %s %v", status, answer, err)
	}

	sent := *got
	if (*header).Get("X-Api-Key") != "sk-test" {
		t.Errorf("key header = %q", (*header).Get("X-Api-Key"))
	}
	if sent["model"] != DefaultClaudeModel || sent["max_tokens"] != float64(900) {
		t.Errorf("model/max_tokens = %v/%v", sent["model"], sent["max_tokens"])
	}
	if _, ok := sent["temperature"]; ok {
		t.Error("temperature was forwarded; the current models refuse it")
	}
	if tc, _ := sent["tool_choice"].(map[string]any); tc["type"] != "auto" {
		t.Errorf("tool_choice = %v, want auto (forced choices are refused)", sent["tool_choice"])
	}
	if stops, _ := sent["stop_sequences"].([]any); len(stops) != 1 || stops[0] != "END" {
		t.Errorf("stop_sequences = %v", sent["stop_sequences"])
	}
	if sys, _ := sent["system"].([]any); len(sys) != 1 || !strings.Contains(mustJSON(sys), "You are TheBot.") {
		t.Errorf("system = %v", sent["system"])
	}

	msgs, _ := sent["messages"].([]any)
	roles := []string{}
	for _, m := range msgs {
		roles = append(roles, m.(map[string]any)["role"].(string))
	}
	// user, assistant (tool_use), user (tool_result + the next question, merged)
	if strings.Join(roles, ",") != "user,assistant,user" {
		t.Fatalf("roles = %v (turns must alternate)", roles)
	}
	all := mustJSON(msgs)
	for _, want := range []string{`"type":"image"`, `"media_type":"image/png"`, `"type":"tool_use"`, `"path":"a.pdf"`, `"type":"tool_result"`, `"tool_use_id":"tu_1"`, "Invoice INV-7", "And the next one?"} {
		if !strings.Contains(all, want) {
			t.Errorf("messages lack %s: %s", want, all)
		}
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(answer, &out); err != nil || len(out.Choices) != 1 {
		t.Fatalf("answer = %s (%v)", answer, err)
	}
	ch := out.Choices[0]
	if ch.Message.Content != "Reading it." || ch.FinishReason != "tool_calls" || len(ch.Message.ToolCalls) != 1 {
		t.Fatalf("choice = %+v", ch)
	}
	if call := ch.Message.ToolCalls[0]; call.ID != "tu_2" || call.Function.Name != "read_document" || !strings.Contains(call.Function.Arguments, "b.pdf") {
		t.Errorf("tool call = %+v", call)
	}
	if out.Usage.PromptTokens != 120 || out.Usage.CompletionTokens != 30 {
		t.Errorf("usage = %+v", out.Usage)
	}
}

// An agent's turns re-send the same conversation: the system prompt and the end of the last message are marked for
// Claude's prompt cache, and a cached read is counted as prompt tokens and said as cached ones.
func TestClaudeIsAskedToCacheThePrefixAndSaysWhatWasCached(t *testing.T) {
	srv, got, _ := fakeMessagesAPI(t, 200, `{
		"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5",
		"content":[{"type":"text","text":"COMMAND:\nls"}],"stop_reason":"end_turn",
		"usage":{"input_tokens":50,"output_tokens":10,"cache_read_input_tokens":9000,"cache_creation_input_tokens":400}}`, nil)
	c := NewClaude("sk-test", "", "")
	c.BaseURL = srv.URL
	body := `{"model":"default","messages":[{"role":"system","content":"You are TheBot."},
	  {"role":"user","content":"The contract and the task."},{"role":"assistant","content":"COMMAND:\nls"},
	  {"role":"user","content":"Command execution result: a.txt"}]}`
	status, answer, _, err := c.Chat(context.Background(), []byte(body))
	if err != nil || status != 200 {
		t.Fatalf("Chat = %d %s %v", status, answer, err)
	}
	sent := *got
	sys := sent["system"].([]any)
	if mustJSON(sys[len(sys)-1]) == "" || !strings.Contains(mustJSON(sys[len(sys)-1]), `"cache_control":{"type":"ephemeral"`) {
		t.Errorf("system not marked: %s", mustJSON(sys))
	}
	msgs := sent["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)["content"].([]any)
	if !strings.Contains(mustJSON(last[len(last)-1]), `"cache_control":{"type":"ephemeral"`) {
		t.Errorf("the last message is not marked: %s", mustJSON(last))
	}
	if strings.Count(mustJSON(msgs), "cache_control") != 1 {
		t.Errorf("only the last block is marked: %s", mustJSON(msgs))
	}
	var out struct {
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
			Details      struct {
				Cached int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(answer, &out); err != nil {
		t.Fatal(err)
	}
	if out.Usage.PromptTokens != 9450 || out.Usage.Details.Cached != 9000 {
		t.Errorf("usage = %+v, want every prompt token counted and the cached ones said", out.Usage)
	}
}

func TestClaudePassesARateLimitThroughForTheDesktopToWaitOut(t *testing.T) {
	srv, _, _ := fakeMessagesAPI(t, 429,
		`{"type":"error","error":{"type":"rate_limit_error","message":"Number of request tokens has exceeded your per-minute rate limit"}}`,
		http.Header{"Retry-After": {"17"}})

	c := NewClaude("sk-test", "", "")
	c.BaseURL = srv.URL

	status, answer, retry, err := c.Chat(context.Background(), []byte(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil || status != 429 {
		t.Fatalf("Chat = %d %s %v", status, answer, err)
	}
	if retry.Get("Retry-After") != "17" {
		t.Errorf("retry headers = %v", retry)
	}
	if !strings.Contains(string(answer), "per-minute rate limit") {
		t.Errorf("answer = %s", answer)
	}
}

func TestClaudeRefusesAModelItDoesNotOffer(t *testing.T) {
	c := NewClaude("sk-test", "", "")

	if _, _, _, err := c.Chat(context.Background(), []byte(`{"model":"claude-fable-5-1","messages":[{"role":"user","content":"hi"}]}`)); err == nil {
		t.Fatal("an unoffered model was forwarded")
	}
}

func TestHubSavesKeysSealedAndNeverShowsThem(t *testing.T) {
	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	key, err := sealed.LoadKey(t.TempDir(), KeyFile)
	if err != nil {
		t.Fatal(err)
	}

	hub, err := NewHub(db, key)
	if err != nil {
		t.Fatal(err)
	}

	if hub.AnyConfigured() {
		t.Fatal("a new hub says something is set up")
	}
	if err := hub.Save(ProviderClaude, Settings{}); err == nil {
		t.Fatal("saved without a key")
	}
	if err := hub.Save(ProviderAzure, Settings{Endpoint: "http://plain.example", Key: "k"}); err == nil {
		t.Fatal("saved an Azure endpoint that is not https")
	}

	if err := hub.Save(ProviderClaude, Settings{Key: "sk-ant-secret"}); err != nil {
		t.Fatal(err)
	}

	var stored string
	if err := db.QueryRow(`SELECT api_key FROM llm_providers WHERE provider = 'claude'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "sk-ant-secret") || !sealed.IsSealed(stored) {
		t.Fatalf("stored key = %q, want it sealed", stored)
	}

	if strings.Contains(mustJSON(hub.Status()), "sk-ant") {
		t.Fatal("Status shows the key")
	}

	// Changing the model without retyping the key keeps the key.
	if err := hub.Save(ProviderClaude, Settings{Model: "claude-sonnet-5-5"}); err != nil {
		t.Fatal(err)
	}

	again, err := NewHub(db, key)
	if err != nil {
		t.Fatal(err)
	}
	st := again.Status()[ProviderClaude]
	if !st.Configured || st.Model != "claude-sonnet-5-5" || st.FastModel != DefaultClaudeFastModel {
		t.Fatalf("reloaded status = %+v", st)
	}
	if again.savedKey(ProviderClaude) != "sk-ant-secret" {
		t.Fatal("the key did not survive a reload")
	}

	if err := again.Forget(ProviderClaude); err != nil || again.Configured(ProviderClaude) {
		t.Fatalf("Forget: %v, configured %v", err, again.Configured(ProviderClaude))
	}
	if _, _, _, err := again.Chat(context.Background(), ProviderClaude, []byte(`{}`)); err != ErrNotConfigured {
		t.Fatalf("chat after forget: %v", err)
	}
}

func mustJSON(v any) string {
	out, _ := json.Marshal(v)
	return string(out)
}

func TestTestFindsOutWhetherTheModelReadsImages(t *testing.T) {
	// Azure deployments: one text-only (refuses a picture), one that sees.
	azure := func(seesPictures bool) *httptest.Server {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			if strings.Contains(string(raw), "image_url") && !seesPictures {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"message":"image input is not supported"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
		}))
		t.Cleanup(srv.Close)
		return srv
	}

	for _, sees := range []bool{false, true} {
		srv := azure(sees)
		hub, _ := NewHub(nil, nil)
		hub.HTTP = srv.Client()

		vision, err := hub.Test(context.Background(), ProviderAzure, Settings{Endpoint: srv.URL, Key: "k"})
		if err != nil || vision != sees {
			t.Errorf("azure that sees=%v: vision %v, err %v", sees, vision, err)
		}
	}

	// Claude: every current model reads images.
	claude, _, _ := fakeMessagesAPI(t, 200, `{"id":"m","type":"message","role":"assistant","model":"x","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, nil)
	hub, _ := NewHub(nil, nil)
	hub.ClaudeBaseURL = claude.URL
	vision, err := hub.Test(context.Background(), ProviderClaude, Settings{Key: "sk"})
	if err != nil || !vision {
		t.Fatalf("claude test = %v, %v", vision, err)
	}
	if err := hub.Save(ProviderClaude, Settings{Key: "sk", Vision: vision}); err != nil {
		t.Fatal(err)
	}
	if !hub.Status()[ProviderClaude].Vision {
		t.Error("status does not say Claude reads images")
	}
}

func TestVisionSurvivesAReload(t *testing.T) {
	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	key := make([]byte, 32)
	hub, _ := NewHub(db, key)
	if err := hub.Save(ProviderAzure, Settings{Endpoint: "https://x.example", Key: "k", Vision: false}); err != nil {
		t.Fatal(err)
	}
	again, _ := NewHub(db, key)
	if st := again.Status()[ProviderAzure]; !st.Configured || st.Vision {
		t.Fatalf("reloaded azure = %+v, want configured and text-only", st)
	}
	if err := again.Save(ProviderAzure, Settings{Vision: true}); err != nil {
		t.Fatal(err)
	}
	third, _ := NewHub(db, key)
	if !third.Status()[ProviderAzure].Vision {
		t.Fatal("vision was not saved")
	}
}

// Without a key file to seal it, an API key is not stored at all - never in plain text (C-19).
func TestAKeyIsNeverSavedUnsealed(t *testing.T) {
	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	hub, err := NewHub(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := hub.Save(ProviderClaude, Settings{Key: "sk-ant-plain-value"}); !errors.Is(err, ErrUnsealed) {
		t.Fatalf("Save without a sealing key = %v, want ErrUnsealed", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM llm_providers`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d rows stored (%v)", n, err)
	}
}
