package httpapi

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/YKoustubhRao/flowera/internal/conversation"
	"github.com/YKoustubhRao/flowera/internal/desktop"
	"github.com/YKoustubhRao/flowera/internal/helpnotes"
	"github.com/YKoustubhRao/flowera/internal/tasks"
	"github.com/YKoustubhRao/flowera/internal/webui"
)

// helpTextHandler records a help note submitted from inside a desktop.
func (s *Server) helpTextHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		DesktopID string `json:"desktop_id"`
		VMName    string `json:"vm_name"`
		Filename  string `json:"filename"`
		Content   string `json:"content"`

		// Kind and Message are the chat's own view of this callback: what
		// sort of message it is, and just the part a human wants to read,
		// without the formatted note's headings. Sent by desktops built
		// from a current image; absent from older ones, which the handler
		// falls back to parsing out of Content.
		Kind    string `json:"kind"`
		Message string `json:"message"`

		// What the task records need from the end of a turn: which task it
		// was, and what the desktop knows about the model and the spend so
		// far. All optional, and absent from older images.
		TaskID       string   `json:"task_id"`
		Engine       string   `json:"engine"`
		Model        string   `json:"model"`
		CostUSD      *float64 `json:"cost_usd"`
		InputTokens  *int64   `json:"input_tokens"`
		OutputTokens *int64   `json:"output_tokens"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if req.DesktopID == "" {
		http.Error(w, "desktop_id is required", http.StatusBadRequest)
		return
	}

	if len(req.Content) > 16384 {
		http.Error(w, "content exceeds maximum size of 16384 bytes", http.StatusBadRequest)
		return
	}

	note := helpnotes.HelpNote{
		ID:        fmt.Sprintf("%d", time.Now().UnixNano()),
		DesktopID: req.DesktopID,
		VMName:    req.VMName,
		Filename:  req.Filename,
		Content:   req.Content,
		CreatedAt: time.Now(),
	}

	s.HelpNotes.Add(note)

	// The same callback feeds two surfaces: the Help rail keeps the whole
	// formatted note, while the chat shows only the agent's actual words.
	chatText, notedKind := agentMessageFromNote(req.Content)

	if trimmed := strings.TrimSpace(req.Message); trimmed != "" {
		chatText = trimmed
	}

	chatKind := chatKindFor(req.Kind)
	if chatKind == "" {
		chatKind = notedKind
	}

	desktopID := desktop.SanitizeID(req.DesktopID)

	taskID, err := s.Tasks.RecordOutcome(desktopID, reportedTaskID(req.TaskID), tasks.Outcome{
		Kind:         outcomeKindFor(req.Kind, chatKind),
		Message:      chatText,
		Engine:       clip(req.Engine, 32),
		Model:        clip(req.Model, 128),
		CostUSD:      nonNegativeFloat(req.CostUSD),
		InputTokens:  nonNegativeInt(req.InputTokens),
		OutputTokens: nonNegativeInt(req.OutputTokens),
	})
	if err != nil {
		log.Printf("helptext from %s: %v", desktopID, err)
	}

	if chatText != "" {
		s.Chat.AppendAgentMessage(
			desktopID,
			taskID,
			chatKind,
			chatText,
		)
	}

	w.Header().Set("Content-Type", "application/json")

	json.NewEncoder(w).Encode(map[string]string{
		"status": "received",
	})
}

// outcomeKindFor maps the kind a desktop reports on its callback to the task
// records' outcome. An older image reports no kind, so the chat kind read off
// its note decides - where an approval and a plain help request look alike.
func outcomeKindFor(reported, chatKind string) string {
	switch strings.ToLower(strings.TrimSpace(reported)) {
	case "approval":
		return tasks.OutcomeApproval
	case "help":
		return tasks.OutcomeHelp
	case "error":
		return tasks.OutcomeError
	case "final", "message":
		return tasks.OutcomeDone
	}

	switch chatKind {
	case conversation.KindApproval:
		return tasks.OutcomeHelp
	case conversation.KindError:
		return tasks.OutcomeError
	default:
		return tasks.OutcomeDone
	}
}

func nonNegativeFloat(value *float64) *float64 {
	if value == nil || *value < 0 {
		return nil
	}

	return value
}

func nonNegativeInt(value *int64) *int64 {
	if value == nil || *value < 0 {
		return nil
	}

	return value
}

// helpNotesHandler returns all recorded help notes as JSON.
func (s *Server) helpNotesHandler(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, s.HelpNotes.List())
}

// closeHelpNoteHandler deletes a help note by ID.
func (s *Server) closeHelpNoteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	id := r.FormValue("id")

	s.HelpNotes.Close(id)

	jsonResponse(w, map[string]string{
		"status": "closed",
	})
}

// dashboardPostsHandler renders the help-notes dashboard, most recent first.
func (s *Server) dashboardPostsHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/dashboard" {
		http.NotFound(w, r)
		return
	}

	notes := s.HelpNotes.List()

	sort.Slice(notes, func(i, j int) bool {
		return notes[i].CreatedAt.After(notes[j].CreatedAt)
	})

	data := webui.HelpNotesPageData{
		Notes: notes,
	}

	if err := s.Web.RenderHelpNotesDashboard(w, data); err != nil {
		http.Error(
			w,
			err.Error(),
			http.StatusInternalServerError,
		)
		return
	}
}
