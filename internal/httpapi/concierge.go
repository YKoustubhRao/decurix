package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/YKoustubhRao/decurix/internal/concierge"
	"github.com/YKoustubhRao/decurix/internal/conversation"
	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/querynotes"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// ConciergeDeps wires internal/concierge to this server: it sends through
// submitRequestWithFiles, the same door the chat page uses (Plan, deliver, Commit),
// so a job started from the concierge is an ordinary task in every record,
// and it stops a desktop the way the dashboard's Stop does.
func (s *Server) ConciergeDeps() concierge.Deps {
	deps := concierge.Deps{
		Desktops: func(ctx context.Context) ([]desktop.Desktop, error) { return s.Desktops.List(ctx) },
		Busy:     func(id string) (string, error) { return s.Tasks.Busy(desktop.SanitizeID(id)) },
		Send: func(ctx context.Context, id, content string) (string, string, error) {
			route, _, err := s.submitRequestWithFiles(ctx, id, content, nil)
			return route.TaskID, route.Kind, err
		},
		Transcript: func(id string) conversation.Thread { return s.Chat.Snapshot(id) },
		TaskStatus: func(taskID string) (string, error) {
			d, err := s.Tasks.Get(taskID)
			return d.Status, err
		},
		TaskOutcome: func(taskID string) (tasks.Display, error) {
			d, err := s.Tasks.Get(taskID)
			return tasks.DisplayOutcome(d.Status, d.Checked), err
		},
		OpenQuestion: s.Tasks.OpenQuestion,
		Stop: func(ctx context.Context, id string) error {
			// The same lifecycle lock as the dashboard's Stop and every delivery (B-23, RW1-E-02).
			unlock := s.Desktops.LockDesktop(id)
			defer unlock()
			return s.thenClose(s.Desktops.Stop, "The desktop was stopped before this task finished.")(ctx, id)
		},
	}
	if s.Suggest != nil {
		deps.Suggest = func(ctx context.Context, content string) (string, bool) {
			sug, ok := s.Suggest.Suggest(ctx, content)
			return sug.DesktopID, ok
		}
	}
	return deps
}

// conciergePageHandler renders the chat-first page (/chat).
func (s *Server) conciergePageHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/chat" {
		http.NotFound(w, r)
		return
	}
	if err := s.Web.RenderConcierge(w); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// conciergeStateHandler returns the conversation after ?after=<message id>
// and the recent sessions, each running one with its live step. Polled by
// the page (the tunnel does not deliver SSE - see the chat page's gotcha).
func (s *Server) conciergeStateHandler(w http.ResponseWriter, r *http.Request) {
	if s.Concierge == nil {
		jsonStatusError(w, http.StatusServiceUnavailable, "the concierge is not running")
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)

	msgs, err := s.Concierge.Store.Messages(after, 300)
	if err != nil {
		jsonError(w, err)
		return
	}
	sessions, err := s.Concierge.Sessions(30)
	if err != nil {
		jsonError(w, err)
		return
	}
	if msgs == nil {
		msgs = []concierge.Message{}
	}
	if sessions == nil {
		sessions = []concierge.Session{}
	}
	for i := range sessions {
		sessions[i].ApprovalNote = s.boundApprovalNote(sessions[i])
	}
	jsonResponse(w, map[string]any{"messages": msgs, "sessions": sessions})
}

// boundApprovalNote is the note a job's Approve/Deny buttons answer: its desktop's newest open question, when that is
// an approval its gate worded. "" otherwise - the page then says to answer it on its card, since a button that cannot
// name the action it approves must not look like it approves one (W-03).
func (s *Server) boundApprovalNote(sess concierge.Session) string {
	if sess.Status != concierge.StatusNeedsYou || sess.Kind != conversation.KindApproval {
		return ""
	}
	open := s.openQuestions(desktop.SanitizeID(sess.DesktopID))
	if len(open) == 0 || open[0].Kind != querynotes.KindApproval || open[0].GateRequest == "" {
		return ""
	}
	return open[0].ID
}

// conciergeAnswerHandler answers a job's approval from its Approve or Deny button (POST session, answer=yes|no,
// note_id): the reply is bound to that note exactly as a reply under its dashboard card is (replyBinding), and
// carries who answered (ActorContext) for the approval policy. Without the note it grants nothing and says where to
// answer instead.
func (s *Server) conciergeAnswerHandler(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}
	if s.Concierge == nil {
		jsonStatusError(w, http.StatusServiceUnavailable, "the concierge is not running")
		return
	}
	n, err := strconv.ParseInt(r.FormValue("session"), 10, 64)
	answer := strings.TrimSpace(r.FormValue("answer"))
	if err != nil || (answer != "yes" && answer != "no") {
		jsonStatusError(w, http.StatusBadRequest, "say which job, and yes or no")
		return
	}
	sess, err := s.Concierge.Store.Session(n)
	if err != nil {
		jsonStatusError(w, http.StatusNotFound, "no such job")
		return
	}
	noteID := strings.TrimSpace(r.FormValue("note_id"))
	if noteID == "" || noteID != s.boundApprovalNote(sess) {
		jsonStatusError(w, http.StatusConflict, "This approval can't be answered here - answer it on its card on the dashboard.")
		return
	}
	ctx := context.WithValue(s.ActorContext(r), replyNoteKey{}, noteID)
	if err := s.Concierge.Answer(context.WithoutCancel(ctx), n, answer == "yes"); err != nil {
		if errors.Is(err, concierge.ErrNotWaitingApproval) {
			jsonStatusError(w, http.StatusConflict, "That job is no longer waiting on this approval.")
			return
		}
		jsonError(w, err)
		return
	}
	jsonResponse(w, map[string]string{"status": "sent"})
}

// conciergeSendHandler takes what a person typed (form field "text").
func (s *Server) conciergeSendHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if s.Concierge == nil {
		jsonStatusError(w, http.StatusServiceUnavailable, "the concierge is not running")
		return
	}
	text := r.FormValue("text")
	if len(text) > 8000 {
		jsonStatusError(w, http.StatusBadRequest, "message is too long")
		return
	}
	// Queued, not processed here: waking a stopped desktop can take a minute. Typed words answer no approval
	// (replyBinding); the bound Approve/Deny buttons go through conciergeAnswerHandler, with who answered.
	msg, err := s.Concierge.Handle(context.WithoutCancel(s.ActorContext(r)), text)
	if err != nil {
		jsonStatusError(w, http.StatusBadRequest, "type a message first")
		return
	}
	jsonResponse(w, map[string]any{"message": msg})
}
