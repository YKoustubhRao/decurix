package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
)

// gateRecorder stands in for a desktop's gate port: it records each path and body, answers each path with the status
// set for it (204 by default), and can hold /task-ended until the test lets go.
type gateRecorder struct {
	mu      sync.Mutex
	calls   []gateCall
	status  map[string]int
	hold    chan struct{}
	reached chan struct{}
}

type gateCall struct {
	path string
	body map[string]any
}

func (g *gateRecorder) RoundTrip(r *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	g.mu.Lock()
	g.calls = append(g.calls, gateCall{r.URL.Path, body})
	code, hold := g.status[r.URL.Path], g.hold
	g.mu.Unlock()
	if hold != nil && r.URL.Path == "/task-ended" {
		g.reached <- struct{}{}
		<-hold
	}
	if code == 0 {
		code = http.StatusNoContent
	}
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}, Request: r}, nil
}

func (g *gateRecorder) answer(path string, code int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.status == nil {
		g.status = map[string]int{}
	}
	g.status[path] = code
}

func (g *gateRecorder) paths() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for _, c := range g.calls {
		out = append(out, c.path)
	}
	return out
}

// gatedManager is a Manager whose one desktop "box" runs, labelled as gated, with gate as its gate port. inspect,
// when set, replaces the running desktop's inspect.
func gatedManager(gate http.RoundTripper, inspect func(string) (container.InspectResponse, error)) *Manager {
	fake := &fakeAPI{}
	fake.inspect = inspect
	if fake.inspect == nil {
		fake.inspect = func(string) (container.InspectResponse, error) {
			return container.InspectResponse{
				ContainerJSONBase: &container.ContainerJSONBase{ID: "cid-box", State: &container.State{Running: true}},
				Config:            &container.Config{Labels: map[string]string{GateLabel: "1"}},
				NetworkSettings: &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{
					NetworkName: {IPAddress: "10.0.0.5"},
				}},
			}, nil
		}
	}
	m := newManagerWith(fake)
	m.HostGUISecret = []byte("secret")
	m.httpClient = &http.Client{Transport: gate}
	return m
}

// The task-ended signature agrees with the vector computed by the Python gate's own MAC (gate_server._mac over
// "task-ended", task, time, nonce), and its label keeps it from standing for a request or a reply.
func TestTaskEndedSignatureMatchesTheSharedVector(t *testing.T) {
	var f struct {
		Key   string `json:"key"`
		Ended struct {
			TaskID string `json:"task_id"`
			Ts     string `json:"ts"`
			Nonce  string `json:"nonce"`
			Sig    string `json:"sig"`
		} `json:"task_ended"`
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "gate-signatures.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	e := f.Ended
	if e.Sig == "" {
		t.Fatal("the fixture has no task_ended vector")
	}
	if got := SignGateTaskEnded(f.Key, e.TaskID, e.Ts, e.Nonce); got != e.Sig {
		t.Fatalf("task-ended signature = %s, the gate computes %s", got, e.Sig)
	}
	if e.Sig == SignGateRequest(f.Key, e.TaskID, e.Ts, e.Nonce, "") || e.Sig == SignGateReply(f.Key, e.TaskID, e.Ts, e.Nonce, "", "", "", "") ||
		e.Sig == SignGateSecrets(f.Key, e.TaskID, e.Ts, e.Nonce, nil) {
		t.Fatal("a task-ended notice reads as another kind of signed body")
	}
}

// What reaches the gate is POST /task-ended with the task, a fresh time and nonce, signed with that desktop's key.
func TestATaskEndedNoticeReachesTheGateSigned(t *testing.T) {
	gate := &gateRecorder{}
	m := gatedManager(gate, nil)
	m.queueTaskEnded("box", "t1")
	m.flushTaskEnded(context.Background(), "box")

	if got := gate.paths(); !slices.Equal(got, []string{"/task-ended"}) {
		t.Fatalf("the gate got %v", got)
	}
	b := gate.calls[0].body
	ts, _ := b["ts"].(string)
	nonce, _ := b["nonce"].(string)
	if b["task_id"] != "t1" || ts == "" || len(nonce) != 32 || b["sig"] != SignGateTaskEnded(m.GateKey("box"), "t1", ts, nonce) {
		t.Fatalf("the notice %v is not signed over what it carries", b)
	}
	if left := m.pendingTaskEndedFor("box"); len(left) != 0 {
		t.Fatalf("a notice the gate took is still kept: %v", left)
	}
}

// A gate that does not take the notice (one from before the route answers 404) costs the next delivery nothing: the
// delivery goes through, the notice is kept, and once the gate takes it, it goes before the next delivery.
func TestANoticeTheGateDidNotTakeGoesWithTheNextDelivery(t *testing.T) {
	gate := &gateRecorder{}
	gate.answer("/task-ended", http.StatusNotFound)
	m := gatedManager(gate, nil)
	m.queueTaskEnded("box", "t1")
	m.flushTaskEnded(context.Background(), "box")
	if left := m.pendingTaskEndedFor("box"); !slices.Equal(left, []string{"t1"}) {
		t.Fatalf("a notice the gate refused was not kept: %v", left)
	}

	if err := m.SendGateSecrets(context.Background(), "box", "t2", []string{"Plum7Orbit"}); err != nil {
		t.Fatalf("a notice the gate does not take failed the next delivery: %v", err)
	}
	if left := m.pendingTaskEndedFor("box"); !slices.Equal(left, []string{"t1"}) {
		t.Fatalf("the notice is lost after a second refusal: %v", left)
	}

	gate.answer("/task-ended", http.StatusNoContent)
	if err := m.SendGateRequest(context.Background(), "box", "t2", "sign in"); err != nil {
		t.Fatal(err)
	}
	want := []string{"/task-ended", "/task-ended", "/secrets", "/task-ended", "/request"}
	if got := gate.paths(); !slices.Equal(got, want) {
		t.Fatalf("the gate got %v, want %v", got, want)
	}
	if left := m.pendingTaskEndedFor("box"); len(left) != 0 {
		t.Fatalf("a notice the gate took is still kept: %v", left)
	}
	// A reply carries the retry too.
	gate.answer("/task-ended", http.StatusNotFound)
	m.queueTaskEnded("box", "t2")
	gate.answer("/task-ended", http.StatusNoContent)
	if err := m.SendGateReply(context.Background(), "box", "t3", "yes", "May I?", "approval", ""); err != nil {
		t.Fatal(err)
	}
	if got := gate.paths(); !slices.Equal(got[len(got)-2:], []string{"/task-ended", "/reply"}) {
		t.Fatalf("a reply did not carry the kept notice first: %v", got)
	}
}

// A desktop Docker says it does not have takes none: its gate went with it, so its notices are let go. A daemon that
// did not answer says nothing about the desktop, so its notices are kept.
func TestNoticesForAGoneDesktopAreLetGoAndForAnUnansweringDaemonKept(t *testing.T) {
	gate := &gateRecorder{}
	gone := gatedManager(gate, func(string) (container.InspectResponse, error) { return container.InspectResponse{}, errFakeNotFound })
	gone.queueTaskEnded("box", "t1")
	gone.flushTaskEnded(context.Background(), "box")
	if left := gone.pendingTaskEndedFor("box"); len(left) != 0 {
		t.Fatalf("notices for a desktop Docker does not have were kept: %v", left)
	}

	slow := gatedManager(gate, func(string) (container.InspectResponse, error) {
		return container.InspectResponse{}, errors.New("context deadline exceeded")
	})
	slow.queueTaskEnded("box", "t1")
	slow.flushTaskEnded(context.Background(), "box")
	if left := slow.pendingTaskEndedFor("box"); !slices.Equal(left, []string{"t1"}) {
		t.Fatalf("a daemon that did not answer lost the notice: %v", left)
	}
	if got := gate.paths(); len(got) != 0 {
		t.Fatalf("a gate was posted to without its desktop: %v", got)
	}
}

// Closing a task never waits on the gate: GateTaskEnded returns while the gate holds the notice, and the notice is
// delivered once the gate answers.
func TestTellingTheGateNeverHoldsUpTheClose(t *testing.T) {
	gate := &gateRecorder{hold: make(chan struct{}), reached: make(chan struct{}, 1)}
	m := gatedManager(gate, nil)
	defer close(gate.hold)

	returned := make(chan struct{})
	go func() {
		m.GateTaskEnded("box", "t1")
		close(returned)
	}()
	// Bounds here only turn a hang into a failure; they are no speed claim.
	select {
	case <-returned:
	case <-time.After(time.Minute):
		t.Fatal("GateTaskEnded waited for the gate")
	}
	select {
	case <-gate.reached:
	case <-time.After(time.Minute):
		t.Fatal("the notice never reached the gate")
	}
	if left := m.pendingTaskEndedFor("box"); !slices.Equal(left, []string{"t1"}) {
		t.Fatalf("a notice the gate has not answered yet is not kept: %v", left)
	}
}

// The notices kept for one gate are bounded; past the bound the oldest goes and the newest are kept.
func TestTheKeptNoticesAreBounded(t *testing.T) {
	m := gatedManager(refusingTransport{}, nil)
	for i := 0; i < maxPendingTaskEnded+10; i++ {
		m.queueTaskEnded("box", fmt.Sprintf("t%d", i))
	}
	left := m.pendingTaskEndedFor("box")
	if len(left) != maxPendingTaskEnded || left[0] != "t10" || left[len(left)-1] != fmt.Sprintf("t%d", maxPendingTaskEnded+9) {
		t.Fatalf("kept %d notices, %s..%s", len(left), left[0], left[len(left)-1])
	}
}

// Deleting a desktop lets go of what was kept for its gate.
func TestDeletingADesktopLetsGoOfItsNotices(t *testing.T) {
	m := gatedManager(refusingTransport{}, nil)
	m.queueTaskEnded("box", "t1")
	if err := m.Delete(context.Background(), "box"); err != nil {
		t.Fatal(err)
	}
	if left := m.pendingTaskEndedFor("box"); len(left) != 0 {
		t.Fatalf("kept %v for a deleted desktop", left)
	}
}
