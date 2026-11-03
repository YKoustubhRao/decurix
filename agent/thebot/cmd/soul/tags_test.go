package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"thebot/internal/config"
	"thebot/internal/llm"
	"thebot/internal/memory"
	"thebot/internal/scheduler"
	"thebot/internal/trace"
	"thebot/internal/workspace"
)

// fakeProxy stands in for the control plane's model proxy: it answers every chat call and keeps its headers.
type fakeProxy struct {
	mu      sync.Mutex
	headers []http.Header
}

func (f *fakeProxy) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.headers = append(f.headers, r.Header.Clone())
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "Hello."}}},
			"usage":   map[string]any{"prompt_tokens": 5, "completion_tokens": 2},
		})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (f *fakeProxy) seen() []http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]http.Header(nil), f.headers...)
}

// newProxiedServer is a soul whose brain is the OpenAI provider pointed at proxy, wrapped as main wraps it.
func newProxiedServer(t *testing.T, proxyURL string) *server {
	t.Helper()
	ws, err := workspace.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l2, err := memory.NewL2(ws.Root())
	if err != nil {
		t.Fatal(err)
	}
	registerTerminalSkillOnce()
	openai := llm.NewOpenAI("k", "")
	openai.BaseURL = proxyURL
	provider := llm.Limited(llm.Metered(trace.Calls(openai)), 2)
	sched := scheduler.New(provider, llm.DefaultModels("m"), ws, l2, nil, nil, nil)
	return newServer(config.Config{OllamaHost: "stub", AgentToken: testAgentToken}, provider, ws, sched)
}

func TestTheTaskIDFromChatReachesEveryCallsHeaders(t *testing.T) {
	proxy := &fakeProxy{}
	s := newProxiedServer(t, proxy.start(t))
	id := postChat(t, s, `{"prompt":"hello, who are you?","taskId":"t_abc-1"}`)
	waitJob(t, s, id, 10*time.Second)

	seen := proxy.seen()
	if len(seen) == 0 {
		t.Fatal("the job made no model call")
	}
	for i, h := range seen {
		if h.Get(llm.HeaderTask) != "t_abc-1" || h.Get(llm.HeaderCaller) != "soul" {
			t.Errorf("call %d: task %q caller %q", i, h.Get(llm.HeaderTask), h.Get(llm.HeaderCaller))
		}
		if p := h.Get(llm.HeaderPurpose); p == "" || p == llm.PurposeUntagged {
			t.Errorf("call %d went out with purpose %q", i, p)
		}
	}
}

func TestAMalformedTaskIDIsIgnored(t *testing.T) {
	proxy := &fakeProxy{}
	s := newProxiedServer(t, proxy.start(t))
	id := postChat(t, s, `{"prompt":"hello, who are you?","taskId":"t 1; rm -rf"}`)
	waitJob(t, s, id, 10*time.Second)
	seen := proxy.seen()
	if len(seen) == 0 {
		t.Fatal("the job made no model call")
	}
	for i, h := range seen {
		if _, ok := h[llm.HeaderTask]; ok {
			t.Errorf("call %d carried a task header %q from a malformed id", i, h.Get(llm.HeaderTask))
		}
	}
	s.jobsMu.Lock()
	job := s.jobs[id]
	s.jobsMu.Unlock()
	if job.TaskID != "" {
		t.Errorf("the job kept the malformed task id %q", job.TaskID)
	}
}

// A job's model calls leave its trace under the task's id: one call line per call the proxy saw.
func TestAJobWritesItsTaskTrace(t *testing.T) {
	proxy := &fakeProxy{}
	s := newProxiedServer(t, proxy.start(t))
	dir := t.TempDir()
	s.tracer = trace.New(dir)
	waitJob(t, s, postChat(t, s, `{"prompt":"hello, who are you?","taskId":"t_trace"}`), 10*time.Second)

	raw, err := os.ReadFile(filepath.Join(dir, "t_trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	calls := strings.Count(string(raw), `"kind":"call"`)
	if calls == 0 || calls != len(proxy.seen()) {
		t.Fatalf("%d call lines for %d calls the proxy saw:\n%s", calls, len(proxy.seen()), raw)
	}
}
