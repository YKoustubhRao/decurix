package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/querynotes"
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

// TestAChatReplyGrantsOnlyTheApprovalItSaw checks which gate request a reply is bound to (round-4 recheck): a yes typed
// in the chat went to the newest open question, so a worker could ask something innocent, then post an approval, and
// the person's yes to the first landed on the second.
func TestAChatReplyGrantsOnlyTheApprovalItSaw(t *testing.T) {
	notes, err := querynotes.NewStore(testDB(t))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{QueryNotes: notes}
	now := time.Now()
	pay := querynotes.QueryNote{ID: "pay", DesktopID: "box", Kind: querynotes.KindApproval, Content: "worker text",
		GateRequest: "r1", GateQuestion: "Approval needed before I click Pay: it pays money.", CreatedAt: now}
	chat := func(seen time.Time) context.Context { return context.WithValue(context.Background(), replySeenKey{}, seen) }
	card := func(id string) context.Context { return context.WithValue(context.Background(), replyNoteKey{}, id) }
	conflict := func(err error) bool {
		var mistake *errUpload
		return errors.As(err, &mistake) && mistake.status == http.StatusConflict
	}

	notes.Add(pay)
	// The only open question, on the person's screen: bound.
	if asked, kind, request, err := s.replyBinding(chat(now.Add(time.Second)), "box"); err != nil || request != "r1" ||
		kind != querynotes.KindApproval || asked != pay.GateQuestion {
		t.Fatalf("chat reply with the approval on screen: %q %q %q %v", asked, kind, request, err)
	}
	// Posted after the newest entry the person had: refused, they had not seen it.
	if _, _, _, err := s.replyBinding(chat(now.Add(-time.Second)), "box"); !conflict(err) {
		t.Fatalf("chat reply to an approval not yet on screen: %v", err)
	}
	// Its card: bound. Another card, or a door that cannot say what was seen: delivered, bound to nothing.
	if _, _, request, _ := s.replyBinding(card("pay"), "box"); request != "r1" {
		t.Fatalf("card reply not bound: %q", request)
	}
	for name, ctx := range map[string]context.Context{"another card": card("old"), "no screen": context.Background()} {
		if _, _, request, err := s.replyBinding(ctx, "box"); err != nil || request != "" {
			t.Fatalf("%s: request %q err %v", name, request, err)
		}
	}

	// An innocent question first, then the approval: a chat yes could mean either, so it is refused.
	notes.Add(querynotes.QueryNote{ID: "ask", DesktopID: "box", Kind: querynotes.KindQuery, Content: "Shall I go on reading the files?",
		CreatedAt: now.Add(-time.Minute)})
	if _, _, _, err := s.replyBinding(chat(now.Add(time.Second)), "box"); !conflict(err) {
		t.Fatalf("chat reply with two open questions: %v", err)
	}
	if _, _, request, _ := s.replyBinding(card("pay"), "box"); request != "r1" {
		t.Fatal("the approval's own card stops binding when another question is open")
	}

	// A plain question newest: as before, the reply reaches it and binds no gate request.
	notes.Add(querynotes.QueryNote{ID: "later", DesktopID: "box", Kind: querynotes.KindQuery, Content: "Which month?",
		CreatedAt: now.Add(time.Minute)})
	if asked, _, request, err := s.replyBinding(chat(now.Add(2*time.Minute)), "box"); err != nil || request != "" || asked != "Which month?" {
		t.Fatalf("a plain question: %q %q %v", asked, request, err)
	}
}
