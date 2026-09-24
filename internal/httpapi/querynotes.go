package httpapi

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/conversation"
	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/querynotes"
	"github.com/YKoustubhRao/decurix/internal/tasks"
	"github.com/YKoustubhRao/decurix/internal/webui"
)

// maxQueryTextRequestBytes bounds one POST /api/querytext body. Content
// itself is capped at 16384 bytes below; this leaves headroom for the
// other fields and stops the full body being buffered before that check
// ever runs.
const maxQueryTextRequestBytes = 32768

// queryTextHandler records a query note submitted from inside a desktop.
func (s *Server) queryTextHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		DesktopID string `json:"desktop_id"`
		VMName    string `json:"vm_name"`
		Filename  string `json:"filename"`
		Content   string `json:"content"`

		// Kind and Reply are the chat's own view of this callback: what sort
		// of entry it is, and just the part a human wants to read, without
		// the formatted note's headings. Sent by desktops built from a
		// current image; absent from older ones, which the handler falls
		// back to parsing out of Content.
		Kind  string `json:"kind"`
		Reply string `json:"reply"`

		// Quiet asks for no Query rail entry: the callback still closes the
		// task and answers in the chat. Sent for a result nobody needs paging
		// about, such as a finished recording, which the desktop card lists.
		Quiet bool `json:"quiet"`

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

	r.Body = http.MaxBytesReader(w, r.Body, maxQueryTextRequestBytes)
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

	note := querynotes.QueryNote{
		ID:        fmt.Sprintf("%d", time.Now().UnixNano()),
		DesktopID: req.DesktopID,
		VMName:    req.VMName,
		Filename:  req.Filename,
		Content:   req.Content,
		CreatedAt: time.Now(),
	}

	// Chat receives every reply. The Query rail keeps formatted notes only
	// for questions, approvals, and failures that need attention.
	chatText, notedKind := agentReplyFromNote(req.Content)

	if trimmed := strings.TrimSpace(req.Reply); trimmed != "" {
		chatText = trimmed
	}

	chatKind := chatKindFor(req.Kind)
	if chatKind == "" {
		chatKind = notedKind
	}

	desktopID := desktop.SanitizeID(req.DesktopID)

	// The kind is what lets a later reply resolve this note: only a query or
	// an approval is answered by one.
	note.Kind = outcomeKindFor(req.Kind, chatKind)

	if !req.Quiet && note.Kind != tasks.OutcomeDone {
		s.QueryNotes.Add(note)
	}

	taskID, err := s.Tasks.RecordOutcome(desktopID, reportedTaskID(req.TaskID), tasks.Outcome{
		Kind:         outcomeKindFor(req.Kind, chatKind),
		Reply:        chatText,
		Engine:       clip(req.Engine, 32),
		Model:        clip(req.Model, 128),
		CostUSD:      nonNegativeFloat(req.CostUSD),
		InputTokens:  nonNegativeInt(req.InputTokens),
		OutputTokens: nonNegativeInt(req.OutputTokens),
	})
	if err != nil {
		log.Printf("querytext from %s: %v", desktopID, err)
	}

	if chatText != "" {
		s.Chat.AppendAgentReply(
			desktopID,
			taskID,
			chatKind,
			chatText,
		)
	}

	if s.IdleChecker != nil && outcomeKindFor(req.Kind, chatKind) == tasks.OutcomeDone {
		s.IdleChecker.FastPauseAfter(desktopID, 15*time.Second)
	}

	w.Header().Set("Content-Type", "application/json")

	json.NewEncoder(w).Encode(map[string]string{
		"status": "received",
	})
}

// outcomeKindFor maps the kind a desktop reports on its callback to the task
// records' outcome. An older image reports no kind, so the chat kind read off
// its note decides - where an approval and a plain query look alike.
func outcomeKindFor(reported, chatKind string) string {
	switch strings.ToLower(strings.TrimSpace(reported)) {
	case "approval":
		return tasks.OutcomeApproval
	case "query":
		return tasks.OutcomeQuery
	case "error":
		return tasks.OutcomeError
	case "final", "message":
		return tasks.OutcomeDone
	}

	switch chatKind {
	case conversation.KindApproval:
		return tasks.OutcomeQuery
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

// queryNotesHandler returns the query notes still open as JSON.
func (s *Server) queryNotesHandler(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, s.QueryNotes.List())
}

// resolvedQueryNotesHandler returns the resolved query notes as JSON, most
// recently resolved first - the Queries page's history.
func (s *Server) resolvedQueryNotesHandler(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, s.QueryNotes.ListResolved())
}

// closeQueryNoteHandler resolves a query note by ID; it stays in the history.
func (s *Server) closeQueryNoteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	id := r.FormValue("id")

	s.QueryNotes.Close(id)

	jsonResponse(w, map[string]string{
		"status": "closed",
	})
}

// dashboardPostsHandler renders the query-notes dashboard, most recent first.
func (s *Server) dashboardPostsHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/dashboard" {
		http.NotFound(w, r)
		return
	}

	notes := s.QueryNotes.List()

	sort.Slice(notes, func(i, j int) bool {
		return notes[i].CreatedAt.After(notes[j].CreatedAt)
	})

	data := webui.QueryNotesPageData{
		Notes: notes,
	}

	if err := s.Web.RenderQueryNotesDashboard(w, data); err != nil {
		http.Error(
			w,
			err.Error(),
			http.StatusInternalServerError,
		)
		return
	}
}
