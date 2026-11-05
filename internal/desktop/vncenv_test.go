package desktop

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The screen's password reaches nothing the agent starts (resource plan B7): the agent's start script takes VNC_PW
// out of its environment before it starts the agent daemon or soul, so no command a model runs inherits it.
func TestTheAgentStartsWithoutTheScreenPassword(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "agent", "start_agent_terminal.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	unset := strings.Index(script, "unset VNC_PW")
	// The agent's launch line, whatever flags python3 takes (round 12 added -I).
	agent := regexp.MustCompile(`python3(\s+-\S+)*\s+/opt/vm_server\.py`).FindStringIndex(script)
	if agent == nil {
		t.Fatalf("no vm_server.py launch line in the start script")
	}
	soul := strings.Index(script, "/opt/soul")
	if unset < 0 || soul < 0 {
		t.Fatalf("unset at %d, agent at %d, soul at %d", unset, agent[0], soul)
	}
	if unset > agent[0] || unset > soul {
		t.Fatal("VNC_PW is unset only after the agent or soul starts")
	}
}
