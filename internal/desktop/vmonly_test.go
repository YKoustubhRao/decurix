// Package desktop: vmonly_test.go checks the VM-only desktop: a brain of "none" runs no agent and names no model.
package desktop

import (
	"os"
	"strings"
	"testing"
)

// A VM-only desktop is told so, has no hosted-model settings, and names no model on its card.
func TestAVMOnlyDesktopRunsNoAgentAndNamesNoModel(t *testing.T) {
	brain := Brain{Kind: BrainNone}
	if !brain.VMOnly() || brain.Hosted() {
		t.Fatalf("VMOnly %v Hosted %v", brain.VMOnly(), brain.Hosted())
	}
	if (Brain{}).VMOnly() || (Brain{Kind: BrainClaude}).VMOnly() {
		t.Fatal("a desktop with a model was taken for a VM only")
	}

	env := buildDesktopEnv("box", HarnessTheBot, "", "", brain)
	if envValue(env, "DECURIX_VM_ONLY") != "1" || envValue(env, "DECURIX_BRAIN") != BrainNone {
		t.Fatalf("env says VM_ONLY=%q BRAIN=%q", envValue(env, "DECURIX_VM_ONLY"), envValue(env, "DECURIX_BRAIN"))
	}
	if got := desktopModel(env); got != "" {
		t.Fatalf("a VM with no model names %q", got)
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "LLM_PROVIDER=") || strings.HasPrefix(kv, "OPENAI_") {
			t.Fatalf("a VM only was given a model setting: %s", kv)
		}
	}

	if envValue(buildDesktopEnv("box", HarnessTheBot, "", "", Brain{}), "DECURIX_VM_ONLY") != "" {
		t.Fatal("a desktop with TheBot was told it is a VM only")
	}
}

// The launcher stops before the agent and soul when told so, and clears the tokens staged for them.
func TestTheLauncherStartsNothingForAVMOnly(t *testing.T) {
	raw, err := os.ReadFile("../../agent/start_agent_terminal.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	stop := strings.Index(script, `DECURIX_VM_ONLY:-}" = "1"`)
	agent := strings.Index(script, "/opt/vm_server.py")
	soul := strings.Index(script, "/opt/soul >>")
	if stop < 0 || agent < 0 || soul < 0 || stop > agent || stop > soul {
		t.Fatalf("the VM-only stop (%d) must come before the agent (%d) and soul (%d)", stop, agent, soul)
	}
	block := script[stop:agent]
	if !strings.Contains(block, "exit 0") || !strings.Contains(block, "agent.token") || !strings.Contains(block, "soul.token") {
		t.Fatalf("the VM-only branch must clear both tokens and exit:\n%s", block)
	}
}
