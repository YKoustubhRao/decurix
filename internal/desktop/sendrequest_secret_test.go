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

	if err := m.SendRequest(context.Background(), "box", RequestBody{Content: "the new password is in the vault", TaskID: "t1", Secret: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.SendRequest(context.Background(), "box", RequestBody{Content: "file the invoices", TaskID: "t2"}); err != nil {
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

// The judgement a reply is and the organisation's date order reach the agent when set, and only then (C6).
func TestARequestCarriesItsJudgementAndTheDateOrder(t *testing.T) {
	fake := &fakeAPI{}
	fake.inspect = func(name string) (container.InspectResponse, error) {
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
	_ = m.SendRequest(context.Background(), "box", RequestBody{Content: "purchases", TaskID: "t1", JudgementID: "j_0123456789abcdef", DateOrder: "day"})
	_ = m.SendRequest(context.Background(), "box", RequestBody{Content: "file it", TaskID: "t2", DateOrder: "sideways"})
	if len(agent.bodies) != 2 {
		t.Fatalf("the agent got %d requests", len(agent.bodies))
	}
	if agent.bodies[0]["judgement_id"] != "j_0123456789abcdef" || agent.bodies[0]["date_order"] != "day" {
		t.Errorf("a reply: %v", agent.bodies[0])
	}
	if _, ok := agent.bodies[1]["judgement_id"]; ok {
		t.Errorf("a new request names a judgement: %v", agent.bodies[1])
	}
	if _, ok := agent.bodies[1]["date_order"]; ok {
		t.Errorf("an unknown date order was sent: %v", agent.bodies[1])
	}
}

// The execution a message belongs to reaches the agent (H-PY-7), and only in the shape the desktop accepts.
func TestARequestCarriesItsExecution(t *testing.T) {
	fake := &fakeAPI{}
	fake.inspect = func(name string) (container.InspectResponse, error) {
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
	_ = m.SendRequest(context.Background(), "box", RequestBody{Content: "a", TaskID: "t1", ExecutionID: "x0123456789abcdef"})
	_ = m.SendRequest(context.Background(), "box", RequestBody{Content: "b", TaskID: "t1", ExecutionID: "x/../etc"})
	if agent.bodies[0]["execution_id"] != "x0123456789abcdef" {
		t.Errorf("the execution was not sent: %v", agent.bodies[0])
	}
	if _, ok := agent.bodies[1]["execution_id"]; ok {
		t.Errorf("an execution id of another shape was sent: %v", agent.bodies[1])
	}
}
