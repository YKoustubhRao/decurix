package desktop

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// The gate (agent/decurix_agent/gate_server.py) verifies with its own implementation of this signature: the two must
// agree byte for byte, so the vector below is computed by the Python side (gate_server.sign) and pinned here.
func TestSignGateReplyMatchesTheGate(t *testing.T) {
	got := SignGateReply("k3y", "task-1", "1700000000", "abcd", "yes, pay it", "Approval needed before I click Pay", "approval", "req-1")
	const want = "d49125da17c706f79b867a210e8895e0652a890caa64d02bd123213f567c3f81"
	if got != want {
		t.Fatalf("signature = %s, the gate computes %s", got, want)
	}
	// Any part changed is another signature: a reply cannot be moved to another task, time, wording or question.
	for name, other := range map[string]string{
		"task":    SignGateReply("k3y", "task-2", "1700000000", "abcd", "yes, pay it", "Approval needed before I click Pay", "approval", "req-1"),
		"time":    SignGateReply("k3y", "task-1", "1700000001", "abcd", "yes, pay it", "Approval needed before I click Pay", "approval", "req-1"),
		"nonce":   SignGateReply("k3y", "task-1", "1700000000", "abce", "yes, pay it", "Approval needed before I click Pay", "approval", "req-1"),
		"text":    SignGateReply("k3y", "task-1", "1700000000", "abcd", "yes", "Approval needed before I click Pay", "approval", "req-1"),
		"asked":   SignGateReply("k3y", "task-1", "1700000000", "abcd", "yes, pay it", "Approval needed before I click Send", "approval", "req-1"),
		"kind":    SignGateReply("k3y", "task-1", "1700000000", "abcd", "yes, pay it", "Approval needed before I click Pay", "query", "req-1"),
		"request": SignGateReply("k3y", "task-1", "1700000000", "abcd", "yes, pay it", "Approval needed before I click Pay", "approval", "req-2"),
		"key":     SignGateReply("k3z", "task-1", "1700000000", "abcd", "yes, pay it", "Approval needed before I click Pay", "approval", "req-1"),
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

// The gate signs its own question (gate_server.sign_question); the control plane verifies it with the same key, over
// the question with its spaces collapsed. The vector is the Python side's.
func TestVerifyGateQuestionMatchesTheGate(t *testing.T) {
	m := &Manager{HostGUISecret: []byte("secret")}
	key := m.GateKey("box-a")
	q := "Approval needed before I click Pay:  it pays money."
	if gateMAC("k3y", "question", "req-1", sha256Hex("Approval needed before I click Pay: it pays money.")) !=
		"aefca23b84ad3ca902b6cb438e1540bed95f62a063357c3208167da360d9f114" {
		t.Fatal("question signature differs from the gate's")
	}
	sig := gateMAC(key, "question", "req-1", sha256Hex("Approval needed before I click Pay: it pays money."))
	if !m.VerifyGateQuestion("box-a", "req-1", q, sig) {
		t.Fatal("the gate's own question did not verify")
	}
	for name, ok := range map[string]bool{
		"other request": m.VerifyGateQuestion("box-a", "req-2", q, sig),
		"other words":   m.VerifyGateQuestion("box-a", "req-1", "Approval needed before I click Save draft: it saves.", sig),
		"other desktop": m.VerifyGateQuestion("box-b", "req-1", q, sig),
		"no signature":  m.VerifyGateQuestion("box-a", "req-1", q, ""),
		"no request id": m.VerifyGateQuestion("box-a", "", q, sig),
	} {
		if ok {
			t.Errorf("%s verified", name)
		}
	}
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
