package httpapi

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/YKoustubhRao/flowera/internal/desktop"
	"github.com/YKoustubhRao/flowera/internal/tasks"
	"github.com/YKoustubhRao/flowera/internal/webui"
)

// maxRequestBytes is the longest request a human can send a desktop.
const maxRequestBytes = 8192

// requestsDashboardHandler renders the requests dashboard page for sending requests to desktops.
func (s *Server) requestsDashboardHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/requests" {
		http.NotFound(w, r)
		return
	}

	desktops, err := s.Desktops.List(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	data := webui.PageData{
		Desktops: desktops,
	}

	if err := s.Web.RenderRequests(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// sendRequestHandler validates and forwards a request to a specific desktop.
func (s *Server) sendRequestHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	desktopID := strings.TrimSpace(r.FormValue("id"))
	content := r.FormValue("content")

	if desktopID == "" {
		http.Error(w, "missing desktop id", http.StatusBadRequest)
		return
	}

	if content == "" {
		http.Error(w, "empty request", http.StatusBadRequest)
		return
	}

	if len(content) > maxRequestBytes {
		http.Error(w, "request too long", http.StatusBadRequest)
		return
	}

	id := desktop.SanitizeID(desktopID)
	route := s.routeRequest(id, content)

	if err := s.Desktops.SendRequest(
		r.Context(),
		desktopID,
		content,
		route.TaskID,
	); err != nil {
		sendRequestError(w, err)
		return
	}

	route = s.recordDelivered(r.Context(), id, content, route)

	jsonResponse(w, map[string]string{
		"status":  "sent",
		"task_id": route.TaskID,
	})
}

// routeRequest decides which task record a human request belongs to before it
// is sent, because the desktop is told the task ID along with the request. A
// records failure never stops a request: it is sent as a new task regardless.
func (s *Server) routeRequest(desktopID, content string) tasks.Route {
	route, err := s.Tasks.Plan(desktopID, content)
	if err != nil {
		log.Printf("sendRequest: %v", err)
		return tasks.NewRoute()
	}

	return route
}

// recordDelivered records a request the desktop has accepted, in the task
// records and in the chat, and returns the route it was recorded under.
//
// Recorded only once the desktop has actually accepted it, so the chat never
// shows a request that was never delivered and no task exists for one. Every
// human-to-desktop path routes through here — the chat page, the requests
// page, and a reply to a query — so all three show up in the transcript and
// the records alike.
func (s *Server) recordDelivered(ctx context.Context, desktopID, content string, route tasks.Route) tasks.Route {
	committed, err := s.Tasks.Commit(route, s.workerFor(ctx, desktopID), content)
	if err != nil {
		log.Printf("sendRequest: %v", err)
	} else {
		route = committed
	}

	s.Chat.AppendUserRequest(desktopID, route.TaskID, content)

	return route
}

// workerFor describes a desktop for its task records: what the records already
// know, or failing that what its container says.
func (s *Server) workerFor(ctx context.Context, desktopID string) tasks.Worker {
	if known, ok := s.Tasks.Worker(desktopID); ok && known.Engine != "" && known.DeletedAt.IsZero() {
		return known
	}

	worker := tasks.Worker{ID: desktopID, Name: desktopID}

	if s.Desktops != nil {
		if found, err := s.Desktops.Get(ctx, desktopID); err == nil {
			worker.Name = found.Name
			worker.Engine = found.Harness
			worker.Profile = found.Profile
			worker.Model = found.Model
		}
	}

	return worker
}

// sendRequestError answers a failed send with the reason a human can act on,
// rather than the blanket 500 every one of these used to get. A desktop whose
// agent has not bound its port yet is the common case and is not an internal
// error: it is a "not ready yet, try again" the chat can show as-is.
func sendRequestError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, desktop.ErrNotFound):
		jsonStatusError(
			w,
			http.StatusNotFound,
			"that desktop no longer exists",
		)

	case errors.Is(err, desktop.ErrNotRunning):
		jsonStatusError(
			w,
			http.StatusConflict,
			"that desktop is stopped - start it and try again",
		)

	case errors.Is(err, desktop.ErrAgentUnreachable):
		jsonStatusError(
			w,
			http.StatusServiceUnavailable,
			"the desktop's agent is not ready yet - it is still starting up, try again in a moment",
		)

	default:
		jsonError(w, err)
		return
	}

	log.Println("sendRequest:", err)
}
