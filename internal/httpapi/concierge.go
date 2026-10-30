package httpapi

import (
	"context"
	"net/http"
	"strconv"

	"github.com/YKoustubhRao/decurix/internal/concierge"
	"github.com/YKoustubhRao/decurix/internal/conversation"
	"github.com/YKoustubhRao/decurix/internal/desktop"
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
		Stop: func(ctx context.Context, id string) error {
			if err := s.Desktops.Stop(ctx, id); err != nil {
				return err
			}
			s.closeOpenTasks(id, "The desktop was stopped before this task finished.")
			return nil
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
	jsonResponse(w, map[string]any{"messages": msgs, "sessions": sessions})
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
	// Queued, not processed here: waking a stopped desktop can take a minute.
	msg, err := s.Concierge.Handle(context.WithoutCancel(r.Context()), text)
	if err != nil {
		jsonStatusError(w, http.StatusBadRequest, "type a message first")
		return
	}
	jsonResponse(w, map[string]any{"message": msg})
}
