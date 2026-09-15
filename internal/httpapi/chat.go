package httpapi

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/YKoustubhRao/flowera/internal/conversation"
	"github.com/YKoustubhRao/flowera/internal/desktop"
	"github.com/YKoustubhRao/flowera/internal/tasks"
)

// maxStepTextBytes bounds one live activity line. Agents report real command
// output, which can be enormous; the chat only ever shows a summary line, so
// anything longer is truncated at the door rather than held in memory.
const maxStepTextBytes = 2048

// maxAgentEventBytes bounds one POST /api/agent/events body, screenshots
// included.
const maxAgentEventBytes = 16 << 20

// stepImagePath is where an action step's screenshots are served from.
const stepImagePath = "/api/desktops/step-image"

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
// control plane's view of the conversation — the task records behind it, and
// the agent's own memory inside the container (worker memory files, and
// whatever its harness keeps), are untouched.
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

	// Screenshots are left in place: the task records behind this chat still
	// point at them as their audit trail.
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
		TaskID    string `json:"task_id"`
		Text      string `json:"text"`
		Action    *struct {
			ID      string `json:"id"`
			Kind    string `json:"kind"`
			Target  string `json:"target"`
			Backend string `json:"backend"`
			Outcome string `json:"outcome"`
			Change  string `json:"change"`
			Reason  string `json:"reason"`
		} `json:"action"`
		BeforePNG []byte `json:"before_png"`
		AfterPNG  []byte `json:"after_png"`
	}

	// Two half-size PNG screenshots, base64-encoded, fit comfortably; this is
	// the ceiling on what one malformed or hostile event can make us read.
	r.Body = http.MaxBytesReader(w, r.Body, maxAgentEventBytes)

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	desktopID := desktop.SanitizeID(strings.TrimSpace(req.DesktopID))
	text := strings.TrimSpace(req.Text)

	if desktopID != "" && text != "" {
		if len(text) > maxStepTextBytes {
			text = text[:maxStepTextBytes] + "…"
		}

		step := tasks.Step{Text: text}

		if req.Action == nil {
			s.Chat.AppendStep(desktopID, text)
		} else {
			action := conversation.Action{
				RecordID: clip(req.Action.ID, 64),
				Kind:     clip(req.Action.Kind, 32),
				Target:   clip(req.Action.Target, maxStepTextBytes),
				Backend:  clip(req.Action.Backend, 32),
				Outcome:  clip(req.Action.Outcome, 32),
				Change:   clip(req.Action.Change, maxStepTextBytes),
				Reason:   clip(req.Action.Reason, maxStepTextBytes),
				Before:   s.stepImageURL(desktopID, req.BeforePNG),
				After:    s.stepImageURL(desktopID, req.AfterPNG),
			}

			s.Chat.AppendActionStep(desktopID, text, action)

			recorded := tasks.Action(action)
			step.Action = &recorded
		}

		// The same step, as the task record's audit trail.
		if _, err := s.Tasks.AddStep(desktopID, reportedTaskID(req.TaskID), step); err != nil {
			log.Printf("agent event from %s: %v", desktopID, err)
		}
	}

	jsonResponse(w, map[string]string{
		"status": "received",
	})
}

// stepImageURL stores one screenshot sent with an action step and returns the
// URL the chat loads it from, or "" when there is none or it is unusable. An
// action whose screenshot is dropped is still recorded - the step is the
// record; the picture is evidence the desktop also keeps on its own disk.
func (s *Server) stepImageURL(desktopID string, data []byte) string {
	if len(data) == 0 || s.StepImages == nil {
		return ""
	}

	id, err := s.StepImages.Put(desktopID, data)
	if err != nil {
		log.Printf("step screenshot from %s dropped: %v", desktopID, err)
		return ""
	}

	return stepImagePath + "?id=" + id
}

// stepImageHandler serves one action step's screenshot. An image never
// changes once stored (a new screenshot gets a new ID), so the browser may
// keep it for good - which matters because the chat rebuilds its whole
// transcript on every snapshot, and would otherwise re-fetch every picture.
func (s *Server) stepImageHandler(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))

	data, ok := s.StepImages.Get(id)
	if id == "" || !ok {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")

	_, _ = w.Write(data)
}

// reportedTaskID is the task ID a desktop echoed back, or "" when it sent none
// or something that is not one - in which case the records attribute the
// report to whatever that desktop is running.
func reportedTaskID(value string) string {
	value = strings.TrimSpace(value)

	if !tasks.ValidID(value) {
		return ""
	}

	return value
}

// clip bounds one reported string field.
func clip(value string, limit int) string {
	value = strings.TrimSpace(value)

	if len(value) > limit {
		return value[:limit] + "…"
	}

	return value
}

// chatKindFor maps the kind an agent reports on its callback to the kind the
// chat renders. An older desktop image sends no kind at all, in which case the
// caller falls back to reading it off the note's own text.
func chatKindFor(reported string) string {
	switch strings.ToLower(strings.TrimSpace(reported)) {
	case "query", "approval":
		return conversation.KindApproval
	case "error":
		return conversation.KindError
	case "final", "message":
		return conversation.KindEntry
	default:
		return ""
	}
}

// queryNoteHeadings are the boilerplate section headings a desktop agent wraps
// its callbacks in (see flowera_agent/messaging.py), paired with the chat kind
// each one implies.
var queryNoteHeadings = []struct {
	Heading string
	Kind    string
}{
	{Heading: "\nFinal reply:\n", Kind: conversation.KindEntry},
	{Heading: "\nProblem:\n", Kind: conversation.KindApproval},
}

// queryNoteTrailers are the boilerplate sections that follow the part a human
// actually wants to read, and so mark where to stop.
var queryNoteTrailers = []string{
	"\n\nCompleted at:",
	"\n\nQuery ID:",
}

// agentReplyFromNote pulls the human-facing sentence out of a query note and
// reports what kind of entry it is.
//
// Desktops built from a current image send that text directly, as its own
// field; this is the fallback for one built before that, whose only channel is
// the formatted note. The note's shape is an agent-side presentation detail
// rather than a contract, which is exactly why it is parsed here as a
// compatibility path and not relied on as the primary one.
func agentReplyFromNote(content string) (text string, kind string) {
	for _, section := range queryNoteHeadings {
		start := strings.Index(content, section.Heading)
		if start == -1 {
			continue
		}

		rest := content[start+len(section.Heading):]

		for _, trailer := range queryNoteTrailers {
			if end := strings.Index(rest, trailer); end != -1 {
				rest = rest[:end]
			}
		}

		return strings.TrimSpace(rest), section.Kind
	}

	return strings.TrimSpace(content), conversation.KindEntry
}
