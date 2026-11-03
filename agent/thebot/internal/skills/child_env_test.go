package skills

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A command the model runs does not see soul's agent token (review T-16).
func TestACommandDoesNotSeeTheAgentToken(t *testing.T) {
	t.Setenv("DECURIX_AGENT_TOKEN", "soul-token-value")
	res := runShell(context.Background(), t.TempDir(), "env", 20*time.Second)
	if res.ExitCode != 0 {
		t.Skipf("no env program here: %s", res.Output)
	}
	if strings.Contains(res.Output, "soul-token-value") || strings.Contains(res.Output, "DECURIX_AGENT_TOKEN") {
		t.Fatal("the agent token reached the command's environment")
	}
}
