package api

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"brainiac/internal/catalog"
	"brainiac/internal/config"
	"brainiac/internal/engine"
	"brainiac/internal/install"
	"brainiac/internal/selector"
	"brainiac/internal/store"
)

func testServer(t *testing.T, key string) *httptest.Server {
	t.Helper()
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ps":
			fmt.Fprint(w, `{"models":[]}`)
		case "/api/chat":
			fmt.Fprintln(w, `{"message":{"content":"Hi "},"done":false}`)
			fmt.Fprintln(w, `{"message":{"content":"there"},"done":false}`)
			fmt.Fprintln(w, `{"message":{"content":""},"done":true,"prompt_eval_count":3,"eval_count":2}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ollama.Close)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lock := &install.Lock{Units: map[string]install.Unit{}, Plan: &selector.Plan{
		Budgets: []selector.Budget{{Pool: "host", BudgetGB: 100}},
		Picks: []selector.Pick{{Role: catalog.RoleBrain, Model: "tiny", Pool: "host", MemGB: 0.01,
			Source: catalog.Source{Kind: catalog.SourceOllama, Runtime: catalog.RuntimeOllama, Tag: "tiny:1b"}}}}}
	cfg := config.Default()
	cfg.Mode, cfg.OllamaURL = config.ModeLocal, ollama.URL
	eng, err := engine.New(engine.Options{Config: cfg, Store: st, Lock: lock})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&Server{Eng: eng, APIKey: key}).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, srv *httptest.Server, path, body, key string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestChatCompletionShape(t *testing.T) {
	srv := testServer(t, "")
	resp := post(t, srv, "/v1/chat/completions", `{"model":"brainiac","messages":[{"role":"user","content":"hello"}]}`, "")
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %v", resp.StatusCode, out)
	}
	msg := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "Hi there" || out["object"] != "chat.completion" {
		t.Fatalf("out %v", out)
	}
	ext := out["brainiac"].(map[string]any)
	if ext["brain"] != "tiny" || ext["trace_id"] == "" {
		t.Fatalf("brainiac %v", ext)
	}
	if out["usage"].(map[string]any)["local_tokens"].(float64) != 5 {
		t.Fatalf("usage %v", out["usage"])
	}
}

func TestStreamSendsDeltasThenTheExtension(t *testing.T) {
	srv := testServer(t, "")
	resp := post(t, srv, "/v1/chat/completions", `{"stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`, "")
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type %q", ct)
	}
	var text strings.Builder
	sawExt, sawUsage, done := false, false, false
	ids := map[string]bool{}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			done = true
			continue
		}
		var ch map[string]any
		json.Unmarshal([]byte(data), &ch)
		ids[ch["id"].(string)] = true
		if _, ok := ch["brainiac"]; ok {
			sawExt = true
		}
		if _, ok := ch["usage"]; ok {
			sawUsage = true
		}
		for _, c := range ch["choices"].([]any) {
			if d, ok := c.(map[string]any)["delta"].(map[string]any)["content"].(string); ok {
				text.WriteString(d)
			}
		}
	}
	if text.String() != "Hi there" || !sawExt || !sawUsage || !done || len(ids) != 1 {
		t.Fatalf("text %q ext %v usage %v done %v ids %v", text.String(), sawExt, sawUsage, done, ids)
	}
}

func TestRefusals(t *testing.T) {
	srv := testServer(t, "")
	cases := map[string]string{
		`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`: "does not fetch URLs",
		`{"messages":[{"role":"user","content":"x"}],"tools":[{"type":"function"}]}`:                                      "tools are not supported",
		`{"messages":[{"role":"tool","content":"x"}]}`:                                                                    "role",
		`{"messages":[]}`: "empty",
		`{"model":"no-such-model","messages":[{"role":"user","content":"x"}]}`: "not installed",
	}
	for body, want := range cases {
		resp := post(t, srv, "/v1/chat/completions", body, "")
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		msg := fmt.Sprint(out["error"])
		if resp.StatusCode < 400 || !strings.Contains(msg, want) {
			t.Errorf("%s -> %d %s (want %q)", body, resp.StatusCode, msg, want)
		}
	}
}

func TestKeyIsRequiredWhenSet(t *testing.T) {
	srv := testServer(t, "s3cret")
	resp := post(t, srv, "/v1/chat/completions", `{"messages":[{"role":"user","content":"x"}]}`, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no key: %d", resp.StatusCode)
	}
	resp = post(t, srv, "/v1/chat/completions", `{"messages":[{"role":"user","content":"x"}]}`, "s3cret")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("with key: %d", resp.StatusCode)
	}
	h, _ := http.Get(srv.URL + "/healthz")
	h.Body.Close()
	if h.StatusCode != 200 {
		t.Fatalf("healthz needs no key: %d", h.StatusCode)
	}
}

func TestInlinePictureIsAccepted(t *testing.T) {
	srv := testServer(t, "")
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nxx"))
	resp := post(t, srv, "/v1/chat/completions", `{"messages":[{"role":"user","content":[{"type":"text","text":"read"},{"type":"image_url","image_url":{"url":"data:image/png;base64,`+png+`"}}]}]}`, "")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		t.Fatalf("status %d %v", resp.StatusCode, out)
	}
}

func TestSniffAudio(t *testing.T) {
	cases := map[string]string{"RIFF0000WAVEfmt ": "wav", "OggS....": "ogg", "fLaC....": "flac", "ID3.....": "mp3", "....ftypM4A ": "m4a"}
	for in, want := range cases {
		if got := sniffAudio([]byte(in)); got != want {
			t.Errorf("%q -> %s want %s", in, got, want)
		}
	}
	if formatOf("talk.mp3", nil) != "mp3" || formatOf("x.bin", []byte("RIFF0000WAVE")) != "wav" {
		t.Error("formatOf")
	}
}
