package tasks

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/withhold"
)

// A desktop that reports its screen password (which its worker can read) in a step, an action or its answer leaves
// none of it in the task record (B7 interim, withhold.HoldAlways).
func TestAHeldScreenPasswordReachesNoTaskRecord(t *testing.T) {
	withhold.HoldAlways("screen:box", "Vnc-8kQz")
	t.Cleanup(func() { withhold.ReleaseAlways("screen:box") })
	s := openStore(t, newClock())
	route := send(t, s, worker("box", "ollama"), "what is the screen's password?")
	if ok, err := s.AddStep("box", route.TaskID, Step{Text: "$ cat /proc/77/environ: VNC_PW=Vnc-8kQz",
		Action: &Action{Kind: "type", Target: "field Vnc-8kQz", Change: "typed Vnc-8kQz"}}); !ok || err != nil {
		t.Fatalf("AddStep = %v, %v", ok, err)
	}
	outcome(t, s, "box", route.TaskID, Outcome{Kind: OutcomeDone, Reply: "it is Vnc-8kQz"})
	if b, _ := json.Marshal(get(t, s, route.TaskID)); strings.Contains(string(b), "Vnc-8kQz") {
		t.Fatalf("the record holds the screen password: %s", b)
	}
}
