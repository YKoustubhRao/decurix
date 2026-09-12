package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/flowera/internal/conversation"
	"github.com/YKoustubhRao/flowera/internal/helpnotes"
)

// chatServer builds the slice of Server the chat handlers actually use. They
// never touch the Docker manager — reaching a desktop is sendMessageHandler's
// job, not theirs — so this needs no daemon and no containers.
func chatServer() *Server {
	return &Server{
		HelpNotes: helpnotes.NewStore(),
		Chat:      conversation.NewStore(),
	}
}

func postJSON(t *testing.T, handler http.HandlerFunc, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	handler(rec, req)

	return rec
}

func readThread(t *testing.T, s *Server, id string) conversation.Thread {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/api/desktops/chat?id="+id, nil)
	rec := httptest.NewRecorder()

	s.chatHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("chatHandler: got status %d", rec.Code)
	}

	var thread conversation.Thread

	if err := json.Unmarshal(rec.Body.Bytes(), &thread); err != nil {
		t.Fatalf("decoding thread: %v", err)
	}

	return thread
}

// A whole turn as the control plane sees it: the agent reports steps while it
// works, then reports the answer, which closes the turn and takes the steps
// with it.
func TestAgentTurnBuildsTranscript(t *testing.T) {
	s := chatServer()

	postJSON(t, s.agentEventHandler, "/api/agent/events",
		`{"desktop_id":"research-box","text":"$ ls -la /work"}`)
	postJSON(t, s.agentEventHandler, "/api/agent/events",
		`{"desktop_id":"research-box","text":"exit 0"}`)

	thread := readThread(t, s, "research-box")

	if !thread.Running {
		t.Fatal("expected reported steps to show the desktop as working")
	}

	if len(thread.Steps) != 2 {
		t.Fatalf("expected 2 live steps, got %d", len(thread.Steps))
	}

	postJSON(t, s.helpTextHandler, "/api/helptext",
		`{"desktop_id":"research-box","vm_name":"research-box","filename":"a.txt",
		  "kind":"final","message":"Cleaned up /work.",
		  "content":"FLOWERA TASK COMPLETED\n\nFinal message:\nCleaned up /work.\n\nCompleted at:\nnow"}`)

	thread = readThread(t, s, "research-box")

	if thread.Running {
		t.Fatal("expected the agent's answer to close the turn")
	}

	if len(thread.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(thread.Messages))
	}

	message := thread.Messages[0]

	// The chat shows what the agent said, not the note's boilerplate — the
	// Help rail is what keeps the formatted version.
	if message.Content != "Cleaned up /work." {
		t.Fatalf("expected just the agent's words, got %q", message.Content)
	}

	if message.Kind != conversation.KindMessage {
		t.Fatalf("expected a plain message, got kind %q", message.Kind)
	}

	if len(message.Steps) != 2 {
		t.Fatalf("expected the answer to carry its 2 steps, got %d", len(message.Steps))
	}

	// The same callback still feeds the Help rail, in full.
	if notes := s.HelpNotes.List(); len(notes) != 1 {
		t.Fatalf("expected the help note to still be recorded, got %d", len(notes))
	}
}

// A desktop built before kind/message existed sends only the formatted note,
// so the handler has to recover both from its text.
func TestLegacyCallbackIsParsed(t *testing.T) {
	s := chatServer()

	postJSON(t, s.helpTextHandler, "/api/helptext",
		`{"desktop_id":"research-box","vm_name":"research-box","filename":"b.txt",
		  "content":"FLOWERA HELP REQUEST\n\nTask:\nwire money\n\nProblem:\nMay I send the payment?\n\nHelp ID:\nabc"}`)

	thread := readThread(t, s, "research-box")

	if len(thread.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(thread.Messages))
	}

	message := thread.Messages[0]

	if message.Content != "May I send the payment?" {
		t.Fatalf("expected the problem text alone, got %q", message.Content)
	}

	if message.Kind != conversation.KindApproval {
		t.Fatalf("expected a help note to read as an approval, got kind %q", message.Kind)
	}
}

// A desktop's own spelling of its ID and the dashboard's spelling must land on
// the same thread, or its messages belong to a desktop nobody is looking at.
func TestDesktopIDSpellingIsNormalised(t *testing.T) {
	s := chatServer()

	postJSON(t, s.agentEventHandler, "/api/agent/events",
		`{"desktop_id":"My.Box","text":"hello"}`)

	if got := len(readThread(t, s, "my-box").Steps); got != 1 {
		t.Fatalf("expected My.Box and my-box to be one thread, got %d steps", got)
	}
}

// Progress reporting must never be able to fail a task, so an unusable event
// is accepted and dropped rather than answered with an error.
func TestMalformedEventIsAccepted(t *testing.T) {
	s := chatServer()

	rec := postJSON(t, s.agentEventHandler, "/api/agent/events", `{"desktop_id":"","text":""}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected an unusable event to be accepted, got status %d", rec.Code)
	}

	if got := len(readThread(t, s, "research-box").Steps); got != 0 {
		t.Fatalf("expected nothing to be recorded, got %d steps", got)
	}
}

func TestClearDiscardsTranscript(t *testing.T) {
	s := chatServer()

	postJSON(t, s.agentEventHandler, "/api/agent/events",
		`{"desktop_id":"research-box","text":"hello"}`)

	req := httptest.NewRequest(http.MethodPost, "/api/desktops/chat/clear",
		strings.NewReader("id=research-box"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	s.clearChatHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("clearChatHandler: got status %d", rec.Code)
	}

	thread := readThread(t, s, "research-box")

	if len(thread.Steps) != 0 || thread.Running {
		t.Fatalf("expected a cleared thread, got %+v", thread)
	}
}

// The stream sends a snapshot the moment a client connects, so a chat opened
// mid-task shows the work already in progress rather than waiting for the next
// change to arrive.
func TestStreamSendsSnapshotOnConnect(t *testing.T) {
	s := chatServer()

	postJSON(t, s.agentEventHandler, "/api/agent/events",
		`{"desktop_id":"research-box","text":"$ whoami"}`)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/api/desktops/chat/stream?id=research-box", nil)
	rec := httptest.NewRecorder()

	s.chatStreamHandler(rec, req.WithContext(ctx))

	body := rec.Body.String()

	if !strings.HasPrefix(body, "data: ") {
		t.Fatalf("expected an SSE data frame, got %q", body)
	}

	if !strings.Contains(body, "$ whoami") {
		t.Fatalf("expected the in-flight step in the first frame, got %q", body)
	}

	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("expected an event-stream content type, got %q", got)
	}
}

func TestChatHandlerRequiresID(t *testing.T) {
	s := chatServer()

	req := httptest.NewRequest(http.MethodGet, "/api/desktops/chat", nil)
	rec := httptest.NewRecorder()

	s.chatHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without an id, got %d", rec.Code)
	}
}
