package eval

import (
	"context"
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/desktop"
)

// noticeSandbox is the runner tests' fake sandbox, also keeping the gate notice each /request carried.
type noticeSandbox struct {
	*fakeSandbox
	mu      sync.Mutex
	notices []desktop.GateNotice
	missing int
}

func (n *noticeSandbox) PracticeAgentRequest(ctx context.Context, d desktop.PracticeDesktop, method, path string, body io.Reader) (*http.Response, error) {
	if method == http.MethodPost && path == "/request" {
		notice, ok := desktop.GateNoticeFrom(ctx)
		n.mu.Lock()
		if ok {
			n.notices = append(n.notices, notice)
		} else {
			n.missing++
		}
		n.mu.Unlock()
	}
	return n.fakeSandbox.PracticeAgentRequest(ctx, d, method, path, body)
}

// Every request an eval sends its practice desktop carries the notice its gate is told (desktop.TellGate, in
// PracticeAgentRequest), for that case's task; and only the arm that runs its own pinned agent tree is made with the
// legacy-gate exception.
func TestAnEvalTellsTheGateAndPinsOnlyItsOldArm(t *testing.T) {
	s, sb, _, _, _ := newService(t, diligent)
	ns := &noticeSandbox{fakeSandbox: sb}
	s.Sandbox = ns
	run, err := s.Start(context.Background(), MatrixRequest{SuiteTar: runnerSuite(t), Repeats: 1,
		Arms: []Arm{{Name: "default", Kind: ArmModel}, {Name: "old", Kind: ArmModel, AgentTree: agentTree(t), AgentTreeRev: "eb15981"}}})
	if err != nil {
		t.Fatal(err)
	}
	if run = waitDone(t, s, run.ID); run.Status != RunDone {
		t.Fatalf("run: %+v", run)
	}
	ns.mu.Lock()
	defer ns.mu.Unlock()
	if ns.missing != 0 || len(ns.notices) == 0 {
		t.Fatalf("%d requests carried no gate notice (%d did)", ns.missing, len(ns.notices))
	}
	for _, n := range ns.notices {
		if n.TaskID == "" || (!n.SendCopy && n.Reply == nil) {
			t.Fatalf("a notice tells the gate nothing of its request: %+v", n)
		}
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()
	legacy := map[bool]int{}
	for _, spec := range sb.specs {
		legacy[spec.LegacyGate == (len(spec.AgentTree) > 0)]++
	}
	if len(sb.specs) != 2 || legacy[true] != 2 {
		t.Fatalf("legacy-gate exception per arm wrong: %d specs, %v", len(sb.specs), legacy)
	}
}
