package desktop

import (
	"os"
	"path/filepath"
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
	agent := strings.Index(script, "python3 /opt/vm_server.py")
	soul := strings.Index(script, "/opt/soul")
	if unset < 0 || agent < 0 || soul < 0 {
		t.Fatalf("unset at %d, agent at %d, soul at %d", unset, agent, soul)
	}
	if unset > agent || unset > soul {
		t.Fatal("VNC_PW is unset only after the agent or soul starts")
	}
}
