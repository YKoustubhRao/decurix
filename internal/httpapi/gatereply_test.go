package httpapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/desktop"
)

// gateQuestionSig signs question as desktop id's approval gate does (gate_server.sign_question).
func gateQuestionSig(m *desktop.Manager, id, requestID, question string) string {
	qh := sha256.Sum256([]byte(strings.Join(strings.Fields(question), " ")))
	mac := hmac.New(sha256.New, []byte(m.GateKey(id)))
	for _, part := range []string{"question", requestID, hex.EncodeToString(qh[:])} {
		mac.Write([]byte(part))
		mac.Write([]byte{0})
	}
	return hex.EncodeToString(mac.Sum(nil))
}

// TestApprovalShowsTheGatesWordsAndBindsTheReply checks an approval the gate signed is shown in the gate's words
// (the worker's after them, marked) and that a reply to it is signed with that request; an approval the worker
// worded alone binds no request, so its reply can grant nothing.
func TestApprovalShowsTheGatesWordsAndBindsTheReply(t *testing.T) {
	s := chatServer(t)
	s.Desktops = &desktop.Manager{HostGUISecret: []byte("secret")}
	question := "Approval needed before I click button \"Pay\": it pays money."

	post := func(gate map[string]string, reply string) {
		body := map[string]any{"desktop_id": "box", "kind": "approval", "reply": reply, "content": reply}
		if gate != nil {
			body["gate"] = gate
		}
		raw, _ := json.Marshal(body)
		postJSON(t, s.queryTextHandler, "/api/querytext", string(raw))
	}

	// A forged signature: the worker's words alone, and no request.
	post(map[string]string{"id": "r1", "question": question, "sig": strings.Repeat("0", 64)}, "Shall I save the draft?")
	if asked, _, request := s.openQuestion("box"); request != "" || strings.Contains(asked, "Pay") {
		t.Fatalf("a forged gate question was taken: asked %q, request %q", asked, request)
	}

	post(map[string]string{"id": "r2", "question": question, "sig": gateQuestionSig(s.Desktops, "box", "r2", question)},
		"I am about to pay the vendor.")
	asked, kind, request := s.openQuestion("box")
	if request != "r2" || asked != question || kind != "approval" {
		t.Fatalf("asked %q kind %q request %q", asked, kind, request)
	}
	entries := readThread(t, s, "box").Entries
	last := entries[len(entries)-1].Content
	if !strings.HasPrefix(last, question) || !strings.Contains(last, "The worker's note: I am about to pay the vendor.") {
		t.Fatalf("card text %q", last)
	}

	// The same signature on another desktop's note does not verify.
	raw, _ := json.Marshal(map[string]any{"desktop_id": "other", "kind": "approval", "reply": "x", "content": "x",
		"gate": map[string]string{"id": "r2", "question": question, "sig": gateQuestionSig(s.Desktops, "box", "r2", question)}})
	postJSON(t, s.queryTextHandler, "/api/querytext", string(raw))
	if _, _, request := s.openQuestion("other"); request != "" {
		t.Fatal("one desktop's gate signature bound another desktop's note")
	}
}
