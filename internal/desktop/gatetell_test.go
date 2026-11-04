package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"

	"github.com/YKoustubhRao/decurix/internal/tasks"
	"github.com/YKoustubhRao/decurix/internal/withhold"
)

// practicePorts stands in for a practice desktop's gate (:8083) and agent (:8081): it records what reached each, in
// order, and answers each gate path with the status set for it (204 by default).
type practicePorts struct {
	mu     sync.Mutex
	log    []string
	status map[string]int
	agent  map[string]any
}

func (p *practicePorts) RoundTrip(r *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(r.Body)
	p.mu.Lock()
	defer p.mu.Unlock()
	code := http.StatusOK
	if strings.HasSuffix(r.URL.Host, ":"+GatePort) {
		p.log = append(p.log, "gate "+r.URL.Path)
		code = http.StatusNoContent
		if c := p.status[r.URL.Path]; c != 0 {
			code = c
		}
	} else {
		p.log = append(p.log, "agent "+r.URL.Path)
		_ = json.Unmarshal(raw, &p.agent)
	}
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}, Request: r}, nil
}

func (p *practicePorts) events() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Join(p.log, ",")
}

// practiceGateManager is a Manager whose one practice desktop "practice-r1" (container practice-r1, no desktop_
// one) runs on the practice network, with ports as its gate and its agent.
func practiceGateManager(t *testing.T, ports *practicePorts) (*Manager, PracticeDesktop) {
	t.Helper()
	fake := &fakeAPI{}
	fake.inspect = func(name string) (container.InspectResponse, error) {
		if name != "practice-r1" {
			return container.InspectResponse{}, errFakeNotFound
		}
		return container.InspectResponse{
			ContainerJSONBase: &container.ContainerJSONBase{ID: "cid-practice-r1", State: &container.State{Running: true}},
			Config:            &container.Config{Labels: map[string]string{PracticeLabel: "true", "decurix-practice-run": "r1"}},
			NetworkSettings: &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{
				PracticeNetworkName: {IPAddress: "172.30.0.5"},
			}},
		}, nil
	}
	m := newManagerWith(fake)
	m.HostGUISecret = []byte("secret")
	m.httpClient = &http.Client{Transport: ports}
	saved := agentClient
	agentClient = &http.Client{Transport: ports}
	t.Cleanup(func() { agentClient = saved })
	return m, PracticeDesktop{ContainerID: "cid-practice-r1", Name: "practice-r1", IP: "172.30.0.5"}
}

// practiceRequest is the notice production would make for text as a new request (tasks.RequestForm,
// withhold.HeldValues, RequestNotice): what practice.RequestGateNotice makes.
func practiceRequest(taskID, text string) GateNotice {
	stored, withheld := tasks.RequestForm(text, false)
	return RequestNotice(taskID, text, stored, withheld, withhold.HeldValues(text, stored))
}

const practiceSecretRequest = "Sign in to records.example.test as mira, password: Plum7Orbit, and export the ledger"

func postPractice(m *Manager, ctx context.Context, d PracticeDesktop, taskID, content string) error {
	b, _ := json.Marshal(map[string]string{"content": content, "task_id": taskID})
	resp, err := m.PracticeAgentRequest(ctx, d, http.MethodPost, "/request", strings.NewReader(string(b)))
	if err == nil {
		resp.Body.Close()
	}
	return err
}

// An eval or practice request reaches its practice desktop's gate as production's does, before its agent: the
// values, then the gate's copy, then the agent, which is told the request is secret. A reply goes values, /reply,
// agent. The gate's posts are signed with the key the practice desktop was made with.
func TestAPracticeRequestReachesItsGateBeforeItsAgent(t *testing.T) {
	ports := &practicePorts{}
	m, d := practiceGateManager(t, ports)
	n := practiceRequest("t1", practiceSecretRequest)
	if !n.Withheld || len(n.Values) == 0 || strings.Contains(n.Copy, "Plum7Orbit") {
		t.Fatalf("the notice does not withhold the password: %+v", n)
	}
	if err := postPractice(m, WithGateNotice(context.Background(), n), d, "t1", practiceSecretRequest); err != nil {
		t.Fatal(err)
	}
	if got, want := ports.events(), "gate /secrets,gate /request,agent /request"; got != want {
		t.Fatalf("events = %s, want %s", got, want)
	}
	if ports.agent["secret"] != true || ports.agent["task_id"] != "t1" || ports.agent["content"] != practiceSecretRequest {
		t.Fatalf("the agent got %v", ports.agent)
	}

	ports.log, ports.agent = nil, nil
	reply := ReplyNotice("t1", false, nil, &GateReply{Text: "the March ledger", Asked: "Which ledger?", Kind: "query"})
	if err := postPractice(m, WithGateNotice(context.Background(), reply), d, "t1", "the March ledger"); err != nil {
		t.Fatal(err)
	}
	if got, want := ports.events(), "gate /reply,agent /request"; got != want {
		t.Fatalf("reply events = %s, want %s", got, want)
	}
	if _, ok := ports.agent["secret"]; ok {
		t.Fatalf("a reply nothing of which was withheld was sent as secret: %v", ports.agent)
	}
}

// Nothing reaches a practice agent whose gate was not told: a gate that answers 500, a request with no notice, and
// a notice for another task are each refused, as production refuses them.
func TestAPracticeGateThatIsNotToldMeansNoDelivery(t *testing.T) {
	ports := &practicePorts{status: map[string]int{"/secrets": http.StatusInternalServerError}}
	m, d := practiceGateManager(t, ports)
	err := postPractice(m, WithGateNotice(context.Background(), practiceRequest("t1", practiceSecretRequest)), d, "t1", practiceSecretRequest)
	if !errors.Is(err, ErrGateNotTold) || strings.Contains(ports.events(), "agent") {
		t.Fatalf("a gate 500: %v, events %s", err, ports.events())
	}
	if err := postPractice(m, context.Background(), d, "t1", "export the ledger"); !errors.Is(err, ErrGateNotTold) {
		t.Fatalf("a request with no notice: %v", err)
	}
	if err := postPractice(m, WithGateNotice(context.Background(), practiceRequest("t2", "x")), d, "t1", "x"); !errors.Is(err, ErrGateNotTold) {
		t.Fatalf("a notice for another task: %v", err)
	}
	if strings.Contains(ports.events(), "agent") {
		t.Fatalf("the agent got something: %s", ports.events())
	}
}

// Pinned old code (LegacyGate, recorded by CreatePractice) whose gate answers 404 to /secrets and /request still gets
// its request; the same 404 on a practice desktop made without it, or any other failure on a pinned one, means no
// delivery.
func TestOnlyAPinnedOldArmPassesItsGatesMissingRoutes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		legacy  bool
		status  int
		deliver bool
	}{
		{"pinned, 404", true, http.StatusNotFound, true},
		{"unpinned, 404", false, http.StatusNotFound, false},
		{"pinned, 500", true, http.StatusInternalServerError, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ports := &practicePorts{status: map[string]int{"/secrets": tc.status, "/request": tc.status}}
			m, d := practiceGateManager(t, ports)
			if tc.legacy {
				m.legacyGates = map[string]bool{d.ContainerID: true}
			}
			err := postPractice(m, WithGateNotice(context.Background(), practiceRequest("t1", practiceSecretRequest)), d, "t1", practiceSecretRequest)
			delivered := strings.HasSuffix(ports.events(), "agent /request")
			if delivered != tc.deliver || (err == nil) != tc.deliver {
				t.Fatalf("delivered %v (%v), want %v; events %s", delivered, err, tc.deliver, ports.events())
			}
		})
	}
}

// A desktop that is not a practice one never has the exception, whatever the record says: its 404 still means no
// delivery.
func TestADesktopThatIsNotPracticeNeverPassesA404(t *testing.T) {
	gate := &gateRecorder{}
	gate.answer("/secrets", http.StatusNotFound)
	m := gatedManager(gate, nil)
	m.legacyGates = map[string]bool{"cid-box": true}
	if err := m.TellGate(context.Background(), "box", practiceRequest("t1", practiceSecretRequest)); !errors.Is(err, ErrGateNoRoute) {
		t.Fatalf("a normal desktop's 404 passed: %v", err)
	}
}

// CreatePractice records the exception only for a spec with LegacyGate and an agent tree of its own; RemovePractice
// forgets it.
func TestOnlyALegacySpecWithItsOwnTreeIsRecorded(t *testing.T) {
	tree := tarBody(map[string]string{"decurix_agent/app.py": "x"})
	for _, tc := range []struct {
		spec PracticeSpec
		want bool
	}{
		{PracticeSpec{RunID: "l1", AgentTree: tree, LegacyGate: true}, true},
		{PracticeSpec{RunID: "l2", AgentTree: tree}, false},
		{PracticeSpec{RunID: "l3", LegacyGate: true}, false},
	} {
		m, daemon := practiceManager(t)
		spec := tc.spec
		spec.Image, spec.Harness, spec.Brain, spec.Model, spec.BrokerURL = daemon.image, "thebot", BrainAzure, "m", "http://172.30.0.1:8091"
		got, err := m.CreatePractice(context.Background(), spec)
		if err != nil {
			t.Fatal(err)
		}
		if m.legacyGates[got.ContainerID] != tc.want {
			t.Errorf("%s: recorded %v, want %v", spec.RunID, m.legacyGates[got.ContainerID], tc.want)
		}
		_ = m.RemovePractice(context.Background(), got.ContainerID)
		if m.legacyGates[got.ContainerID] {
			t.Errorf("%s: still recorded after removal", spec.RunID)
		}
	}
}

// A practice task's end reaches its practice desktop's gate too: the hook's practice id finds the practice container.
func TestAPracticeTaskEndReachesItsGate(t *testing.T) {
	ports := &practicePorts{}
	m, _ := practiceGateManager(t, ports)
	m.queueTaskEnded("practice-r1", "t1")
	m.flushTaskEnded(context.Background(), "practice-r1")
	if got := ports.events(); got != "gate /task-ended" {
		t.Fatalf("events = %s", got)
	}
	if left := m.pendingTaskEndedFor("practice-r1"); len(left) != 0 {
		t.Fatalf("kept %v", left)
	}
}
