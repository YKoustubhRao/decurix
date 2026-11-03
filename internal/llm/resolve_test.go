package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestResolveNamesTheDeploymentThatWouldServe(t *testing.T) {
	hub, _ := NewHub(nil, nil)
	if _, err := hub.Resolve(ProviderAzure, "default"); err == nil {
		t.Fatal("Resolve answered for a provider not set up")
	}
	if err := hub.Save(ProviderAzure, Settings{Endpoint: "https://x.example.test", Key: "k", Model: "big-1", FastModel: "small-1"}); err != nil {
		t.Fatal(err)
	}
	for asked, want := range map[string]string{"": "big-1", "default": "big-1", "fast": "small-1", "big-1": "big-1", "small-1": "small-1"} {
		got, err := hub.Resolve(ProviderAzure, asked)
		if err != nil || got != want {
			t.Errorf("Resolve(azure, %q) = %q, %v; want %q", asked, got, err, want)
		}
	}
	if got, err := hub.Resolve(ProviderAzure, "something-else"); err == nil {
		t.Errorf("a foreign name resolved to %q", got)
	}
	if _, err := hub.Resolve("nope", "default"); err == nil {
		t.Error("an unknown provider resolved")
	}
}

func TestOnChangeFiresOncePerSaveAndForget(t *testing.T) {
	hub, _ := NewHub(nil, nil)
	var mu sync.Mutex
	var seen []string
	hub.OnChange(func(p string) {
		// Runs outside the hub's lock: reading the hub here must not deadlock.
		_, _ = hub.Resolve(p, "default")
		mu.Lock()
		seen = append(seen, p)
		mu.Unlock()
	})
	hub.OnChange(nil)

	_ = hub.Save(ProviderAzure, Settings{Endpoint: "https://x.example.test", Key: "k"})
	_ = hub.Save(ProviderClaude, Settings{Key: "k"})
	if err := hub.Save(ProviderAzure, Settings{Endpoint: "http://plain.example.test", Key: "k"}); err == nil {
		t.Fatal("a bad save was accepted")
	}
	_ = hub.Forget(ProviderAzure)

	want := []string{ProviderAzure, ProviderClaude, ProviderAzure}
	if len(seen) != len(want) {
		t.Fatalf("OnChange saw %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("OnChange saw %v, want %v", seen, want)
		}
	}
}

func TestChatResultSaysWhichDeploymentServed(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()

	hub, _ := NewHub(nil, nil)
	hub.HTTP = upstream.Client()
	if err := hub.Save(ProviderAzure, Settings{Endpoint: upstream.URL, Key: "k", Model: "big-1", FastModel: "small-1"}); err != nil {
		t.Fatal(err)
	}
	r, err := hub.ChatResult(context.Background(), ProviderAzure, []byte(`{"model":"fast","messages":[]}`))
	if err != nil || r.Status != 200 || r.Deployment != "small-1" {
		t.Fatalf("ChatResult = %+v, %v", r, err)
	}
	status, body, _, err := hub.Chat(context.Background(), ProviderAzure, []byte(`{"messages":[]}`))
	if err != nil || status != 200 || string(body) != `{"choices":[]}` {
		t.Fatalf("Chat = %d %s %v", status, body, err)
	}
	r, err = hub.ChatResult(context.Background(), ProviderAzure, []byte(`{"model":"elsewhere"}`))
	if err == nil || r.Deployment != "" {
		t.Fatalf("a foreign model gave %+v, %v", r, err)
	}
}
