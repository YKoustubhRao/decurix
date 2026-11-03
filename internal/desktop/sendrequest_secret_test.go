package desktop

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
)

// agentStub is a fake desktop agent: it keeps the body of every request posted to it.
type agentStub struct{ bodies []map[string]any }

func (a *agentStub) RoundTrip(r *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	a.bodies = append(a.bodies, body)
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}, Request: r}, nil
}

// A message the person marked secret tells the agent so ("secret": true); any other carries no such field (F-2).
func TestASecretRequestTellsTheAgent(t *testing.T) {
	fake := &fakeAPI{}
	fake.inspect = func(name string) (container.InspectResponse, error) {
		if name != "desktop_box" {
			return container.InspectResponse{}, errFakeNotFound
		}
		return container.InspectResponse{
			ContainerJSONBase: &container.ContainerJSONBase{ID: "cid-box", State: &container.State{Running: true}},
			NetworkSettings: &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{
				NetworkName: {IPAddress: "10.0.0.5"},
			}},
		}, nil
	}
	agent := &agentStub{}
	m := newManagerWith(fake)
	m.httpClient = &http.Client{Transport: agent}

	if err := m.SendRequest(context.Background(), "box", "the new password is in the vault", "t1", true); err != nil {
		t.Fatal(err)
	}
	if err := m.SendRequest(context.Background(), "box", "file the invoices", "t2", false); err != nil {
		t.Fatal(err)
	}
	if len(agent.bodies) != 2 {
		t.Fatalf("the agent got %d requests", len(agent.bodies))
	}
	if agent.bodies[0]["secret"] != true || agent.bodies[0]["task_id"] != "t1" || agent.bodies[0]["content"] != "the new password is in the vault" {
		t.Errorf("a secret request: %v", agent.bodies[0])
	}
	if _, ok := agent.bodies[1]["secret"]; ok {
		t.Errorf("an ordinary request carries no secret field: %v", agent.bodies[1])
	}
}
