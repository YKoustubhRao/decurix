package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/YKoustubhRao/flowera/internal/conversation"
	"github.com/YKoustubhRao/flowera/internal/desktop"
)

// maxStepTextBytes bounds one live activity line. Agents report real command
// output, which can be enormous; the chat only ever shows a summary line, so
// anything longer is truncated at the door rather than held in memory.
const maxStepTextBytes = 2048

// keepAliveInterval is how often an idle SSE stream emits a comment frame.
// Without it an idle chat can be dropped by an intermediary (the Cloudflare
// quick tunnel among them) that times out a silent connection.
const keepAliveInterval = 25 * time.Second

// chatHandler returns one desktop's chat transcript as JSON. It is what the
// chat page loads first, and what it falls back to polling if the browser
// cannot hold the event stream open.
func (s *Server) chatHandler(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))

	if id == "" {
		http.Error(w, "missing desktop id", http.StatusBadRequest)
		return
	}

	jsonResponse(w, s.Chat.Snapshot(desktop.SanitizeID(id)))
}

// chatStreamHandler streams one desktop's chat as server-sent events: a full
// snapshot on connect, then another every time the thread changes. Whole
// snapshots rather than deltas is deliberate — threads are small and capped,
// and it makes a reconnecting client self-healing with no sequence tracking on
// either side.
func (s *Server) chatStreamHandler(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))

	if id == "" {
		http.Error(w, "missing desktop id", http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	desktopID := desktop.SanitizeID(id)

	// Subscribe before the first send, so a change landing between the two
	// wakes this client instead of being missed.
	updates, unsubscribe := s.Chat.Subscribe(desktopID)
	defer unsubscribe()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Ask any buffering reverse proxy in front of us to pass frames straight
	// through; a buffered event stream arrives all at once, defeating it.
	w.Header().Set("X-Accel-Buffering", "no")

	send := func() bool {
		payload, err := json.Marshal(s.Chat.Snapshot(desktopID))
		if err != nil {
			return false
		}

		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return false
		}

		flusher.Flush()

		return true
	}

	if !send() {
		return
	}

	keepAlive := time.NewTicker(keepAliveInterval)
	defer keepAlive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return

		case <-updates:
			if !send() {
				return
			}

		case <-keepAlive.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}

			flusher.Flush()
		}
	}
}

// clearChatHandler discards one desktop's transcript. It only clears the
// control plane's record of the conversation — the agent's own memory inside
// the container (worker memory files, and whatever its harness keeps) is
// untouched.
func (s *Server) clearChatHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	id := strings.TrimSpace(r.FormValue("id"))

	if id == "" {
		http.Error(w, "missing desktop id", http.StatusBadRequest)
		return
	}

	s.Chat.Clear(desktop.SanitizeID(id))

	jsonResponse(w, map[string]string{
		"status": "cleared",
	})
}

// agentEventHandler records one line of live activity reported from inside a
// desktop while it works. Every harness posts here through the agent's shared
// progress.emit (flowera_agent/progress.py), which is what lets the chat show
// the same step-by-step view for Ollama, Claude Code and TheBot alike.
//
// Best-effort by design: a desktop reporting progress must never be able to
// fail a task, so an unusable event is accepted and dropped rather than
// answered with an error the agent would then have to handle.
func (s *Server) agentEventHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		DesktopID string `json:"desktop_id"`
		Text      string `json:"text"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	desktopID := strings.TrimSpace(req.DesktopID)
	text := strings.TrimSpace(req.Text)

	if desktopID != "" && text != "" {
		if len(text) > maxStepTextBytes {
			text = text[:maxStepTextBytes] + "…"
		}

		s.Chat.AppendStep(desktop.SanitizeID(desktopID), text)
	}

	jsonResponse(w, map[string]string{
		"status": "received",
	})
}

// chatKindFor maps the kind an agent reports on its callback to the kind the
// chat renders. An older desktop image sends no kind at all, in which case the
// caller falls back to reading it off the note's own text.
func chatKindFor(reported string) string {
	switch strings.ToLower(strings.TrimSpace(reported)) {
	case "help", "approval":
		return conversation.KindApproval
	case "error":
		return conversation.KindError
	case "final", "message":
		return conversation.KindMessage
	default:
		return ""
	}
}

// helpNoteHeadings are the boilerplate section headings a desktop agent wraps
// its callbacks in (see flowera_agent/messaging.py), paired with the chat kind
// each one implies.
var helpNoteHeadings = []struct {
	Heading string
	Kind    string
}{
	{Heading: "\nFinal message:\n", Kind: conversation.KindMessage},
	{Heading: "\nProblem:\n", Kind: conversation.KindApproval},
}

// helpNoteTrailers are the boilerplate sections that follow the part a human
// actually wants to read, and so mark where to stop.
var helpNoteTrailers = []string{
	"\n\nCompleted at:",
	"\n\nHelp ID:",
}

// agentMessageFromNote pulls the human-facing sentence out of a help note and
// reports what kind of message it is.
//
// Desktops built from a current image send that text directly, as its own
// field; this is the fallback for one built before that, whose only channel is
// the formatted note. The note's shape is an agent-side presentation detail
// rather than a contract, which is exactly why it is parsed here as a
// compatibility path and not relied on as the primary one.
func agentMessageFromNote(content string) (text string, kind string) {
	for _, section := range helpNoteHeadings {
		start := strings.Index(content, section.Heading)
		if start == -1 {
			continue
		}

		rest := content[start+len(section.Heading):]

		for _, trailer := range helpNoteTrailers {
			if end := strings.Index(rest, trailer); end != -1 {
				rest = rest[:end]
			}
		}

		return strings.TrimSpace(rest), section.Kind
	}

	return strings.TrimSpace(content), conversation.KindMessage
}
