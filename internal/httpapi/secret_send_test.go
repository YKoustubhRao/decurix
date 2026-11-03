package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"

	"github.com/YKoustubhRao/decurix/internal/desktop"
)

// A message the person marks secret in the compose box reaches the desktop's agent with "secret": true, end to end
// from the handler the compose box posts to (/api/desktops/send): the seam F-2 was about. An unmarked one carries no
// such field.
func TestAComposeSecretReachesTheAgentMarked(t *testing.T) {
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

	// The fake daemon reports desktop_box running at the agent stub's address. SendRequest builds
	// "http://<address>:8081/request"; an address ending in "/?" puts the stub's own port first and leaves ":8081/request"
	// as a query the stub ignores, so no real port 8081 is needed.
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
	s := chatServer(t)
	s.Desktops = desktop.NewManager(cli)
	s.Desktops.HostGUISecret = []byte("test secret")

	send := func(form url.Values) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/desktops/send", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		s.sendRequestHandler(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("send: %d %s", rec.Code, rec.Body)
		}
	}
	send(url.Values{"id": {"box"}, "content": {"the vault code is in the drawer"}, "secret": {"1"}})
	send(url.Values{"id": {"box"}, "content": {"file the invoices"}})

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("the agent got %d requests, want 2", len(bodies))
	}
	if bodies[0]["secret"] != true || bodies[0]["content"] != "the vault code is in the drawer" {
		t.Errorf("a marked message reached the agent as %v", bodies[0])
	}
	if _, ok := bodies[1]["secret"]; ok {
		t.Errorf("an unmarked message carries a secret field: %v", bodies[1])
	}
}
