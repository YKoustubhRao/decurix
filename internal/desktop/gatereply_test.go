package desktop

import "testing"

// The gate (agent/decurix_agent/gate_server.py) verifies with its own implementation of this signature: the two must
// agree byte for byte, so the vector below is computed by the Python side (gate_server.sign) and pinned here.
func TestSignGateReplyMatchesTheGate(t *testing.T) {
	got := SignGateReply("k3y", "task-1", "1700000000", "abcd", "yes, pay it", "Approval needed before I click Pay", "approval")
	const want = "87abad8e0ec160fa1d8ba7e729fcc6083e85f3da9e5e65c8a9f47418cc1731ff"
	if got != want {
		t.Fatalf("signature = %s, the gate computes %s", got, want)
	}
	// Any part changed is another signature: a reply cannot be moved to another task, time, wording or question.
	for name, other := range map[string]string{
		"task":  SignGateReply("k3y", "task-2", "1700000000", "abcd", "yes, pay it", "Approval needed before I click Pay", "approval"),
		"time":  SignGateReply("k3y", "task-1", "1700000001", "abcd", "yes, pay it", "Approval needed before I click Pay", "approval"),
		"nonce": SignGateReply("k3y", "task-1", "1700000000", "abce", "yes, pay it", "Approval needed before I click Pay", "approval"),
		"text":  SignGateReply("k3y", "task-1", "1700000000", "abcd", "yes", "Approval needed before I click Pay", "approval"),
		"asked": SignGateReply("k3y", "task-1", "1700000000", "abcd", "yes, pay it", "Approval needed before I click Send", "approval"),
		"kind":  SignGateReply("k3y", "task-1", "1700000000", "abcd", "yes, pay it", "Approval needed before I click Pay", "query"),
		"key":   SignGateReply("k3z", "task-1", "1700000000", "abcd", "yes, pay it", "Approval needed before I click Pay", "approval"),
	} {
		if other == want {
			t.Errorf("changing the %s did not change the signature", name)
		}
	}
}

// The gate key is not the agent token (which the worker's environment holds) and differs per desktop.
func TestGateKeyIsNotTheAgentToken(t *testing.T) {
	m := &Manager{HostGUISecret: []byte("secret")}
	if m.GateKey("box-a") == "" || m.GateKey("box-a") == m.AgentToken("box-a") {
		t.Fatal("the gate key must exist and differ from the agent token")
	}
	if m.GateKey("box-a") == m.GateKey("box-b") {
		t.Fatal("each desktop has its own gate key")
	}
	if (&Manager{}).GateKey("box-a") != "" {
		t.Fatal("no secret, no key")
	}
}
