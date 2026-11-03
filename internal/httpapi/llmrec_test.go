package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"

	"github.com/YKoustubhRao/decurix/internal/blobstore"
	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/llm"
	"github.com/YKoustubhRao/decurix/internal/llmrec"
)

// fakeAgentDesktop points s.Desktops at a stand-in daemon that reports desktop_box running, with its agent's /request
// served by a stub that keeps every body it gets (as TestAComposeSecretReachesTheAgentMarked does).
func fakeAgentDesktop(t *testing.T, s *Server) func() []map[string]any {
	t.Helper()
	var mu sync.Mutex
	var bodies []map[string]any
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(agent.Close)
	stub := strings.TrimPrefix(agent.URL, "http://") + "/?"
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("[]"))
		case strings.HasSuffix(r.URL.Path, "/containers/desktop_box/json"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(container.InspectResponse{
				ContainerJSONBase: &container.ContainerJSONBase{ID: "box", Name: "/desktop_box", State: &container.State{Running: true, Status: "running"}},
				NetworkSettings: &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{
					desktop.NetworkName: {IPAddress: stub},
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(daemon.Close)
	cli, err := client.NewClientWithOpts(client.WithHTTPClient(daemon.Client()),
		client.WithHost("tcp://"+strings.TrimPrefix(daemon.URL, "http://")), client.WithVersion("1.43"))
	if err != nil {
		t.Fatal(err)
	}
	s.Desktops = desktop.NewManager(cli)
	s.Desktops.HostGUISecret = []byte("test secret")
	return func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), bodies...)
	}
}

// recordingServer is chatServer with a hosted Azure model answered by a stand-in, and the call recorder on.
func recordingServer(t *testing.T) *Server {
	t.Helper()
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}],` +
			`"usage":{"prompt_tokens":40,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":32}}}`))
	}))
	t.Cleanup(upstream.Close)
	s := chatServer(t)
	hub, _ := llm.NewHub(nil, nil)
	hub.HTTP = upstream.Client()
	if err := hub.Save(llm.ProviderAzure, llm.Settings{Endpoint: upstream.URL, Key: "k", Model: "big-1", FastModel: "small-1"}); err != nil {
		t.Fatal(err)
	}
	s.LLM = hub
	blobs, err := blobstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if s.LLMRec, err = llmrec.NewStore(testDB(t), blobs); err != nil {
		t.Fatal(err)
	}
	return s
}

// proxyCall is one chat through the proxy as desktop (""  for the host), with the given tag headers.
func proxyCall(t *testing.T, s *Server, provider, desktopID, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/llm/"+provider+"/v1/chat/completions", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if desktopID != "" {
		req = req.WithContext(context.WithValue(req.Context(), callerKey{}, desktopID))
	}
	rec := httptest.NewRecorder()
	s.llmProxyHandler(rec, req)
	return rec
}

// Every hosted call leaves one row with the deployment that served it, its tags, usage and status.
func TestTheProxyRecordsEveryCall(t *testing.T) {
	s := recordingServer(t)
	rec := proxyCall(t, s, "azure", "box", `{"model":"fast","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{llmrec.HeaderPurpose: "operator.turn", llmrec.HeaderTask: "t0123456789abcdef",
			llmrec.HeaderTurn: "j4:2", llmrec.HeaderCaller: "soul"})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "done") {
		t.Fatalf("call: %d %s", rec.Code, rec.Body)
	}
	rows, err := s.LLMRec.Calls("t0123456789abcdef")
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %v, %v", rows, err)
	}
	r := rows[0]
	if r.Deployment != "small-1" || r.DesktopID != "box" || r.Purpose != "operator.turn" || r.Turn != "j4:2" ||
		r.Caller != "soul" || r.Status != 200 || *r.InputTokens != 40 || *r.CachedInputTokens != 32 || r.ResponseSHA == "" {
		t.Fatalf("row = %+v", r)
	}

	// A provider not set up still answers the desktop as before, and leaves a row with that status.
	if rec := proxyCall(t, s, "claude", "box", `{"messages":[]}`, map[string]string{llmrec.HeaderPurpose: "review",
		llmrec.HeaderTask: "t0123456789abcdef"}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("an unset provider: %d", rec.Code)
	}
	rows, _ = s.LLMRec.Calls("t0123456789abcdef")
	if len(rows) != 2 || rows[1].Status != http.StatusServiceUnavailable || rows[1].Deployment != "" {
		t.Fatalf("rows after a hub error = %+v", rows)
	}

	// The read routes: the rows without bodies, a stored message, and the counts.
	w := httptest.NewRecorder()
	s.llmCallsHandler(w, httptest.NewRequest(http.MethodGet, "/api/llm/calls?task=t0123456789abcdef", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"small-1"`) || strings.Contains(w.Body.String(), `"hi"`) {
		t.Fatalf("calls route: %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	s.llmBlobHandler(w, httptest.NewRequest(http.MethodGet, "/api/llm/blobs/"+r.Messages[0], nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"hi"`) {
		t.Fatalf("blob route: %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	s.llmBlobHandler(w, httptest.NewRequest(http.MethodGet, "/api/llm/blobs/../../etc/passwd", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a bad hash: %d", w.Code)
	}
	w = httptest.NewRecorder()
	s.llmCallStatsHandler(w, httptest.NewRequest(http.MethodGet, "/api/llm/calls/stats", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"operator.turn":1`) {
		t.Fatalf("stats route: %d %s", w.Code, w.Body)
	}
}

// A message the person marked secret, sent through the compose box, is in no blob of a model call the desktop makes
// with it afterwards.
func TestAMarkedSecretIsInNoStoredCall(t *testing.T) {
	s := recordingServer(t)
	fakeAgentDesktop(t, s)
	const secret = "Plum-Orbit-7731"
	form := url.Values{"id": {"box"}, "content": {secret}, "secret": {"1"}}
	req := httptest.NewRequest(http.MethodPost, "/api/desktops/send", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.sendRequestHandler(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("send: %d %s", w.Code, w.Body)
	}

	body, _ := json.Marshal(map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "the person said: " + secret},
		map[string]any{"role": "assistant", "content": "I will type Plum-Orbit-7731 into the field"},
	}})
	if rec := proxyCall(t, s, "azure", "box", string(body), map[string]string{llmrec.HeaderPurpose: "operator.turn"}); rec.Code != 200 {
		t.Fatalf("call: %d", rec.Code)
	}
	stored := 0
	_ = filepath.Walk(s.LLMRec.Blobs().Root(), func(p string, info os.FileInfo, err error) error {
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		stored++
		data, _ := os.ReadFile(p)
		if strings.Contains(string(data), secret) {
			t.Errorf("blob %s holds the marked secret: %s", filepath.Base(p), data)
		}
		return nil
	})
	if stored < 2 {
		t.Fatalf("only %d blobs stored", stored)
	}
	// Another desktop's calls are not withheld by this one's secrets (they are not that desktop's to know).
	if got := s.secretsSeen.For("other"); len(got) != 0 {
		t.Fatalf("another desktop holds %v", got)
	}
}

// A recorder that cannot write still lets the call through, and the failure is counted where a person looks.
func TestAFailedRecordingNeverFailsTheCall(t *testing.T) {
	s := recordingServer(t)
	db := testDB(t)
	blobs, _ := blobstore.New(t.TempDir())
	store, err := llmrec.NewStore(db, blobs)
	if err != nil {
		t.Fatal(err)
	}
	s.LLMRec = store
	db.Close()
	if rec := proxyCall(t, s, "azure", "box", `{"messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{llmrec.HeaderPurpose: "review"}); rec.Code != 200 {
		t.Fatalf("call: %d %s", rec.Code, rec.Body)
	}
	w := httptest.NewRecorder()
	s.llmCallStatsHandler(w, httptest.NewRequest(http.MethodGet, "/api/llm/calls/stats", nil))
	if !strings.Contains(w.Body.String(), `"record_failures":1`) {
		t.Fatalf("stats: %d %s", w.Code, w.Body)
	}
}
