package httpapi

import (
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/YKoustubhRao/flowera/internal/desktop"
	"github.com/YKoustubhRao/flowera/internal/webui"
)

// messagesDashboardHandler renders the messages dashboard page for sending messages to desktops.
func (s *Server) messagesDashboardHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/messages" {
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

	if err := s.Web.RenderMessages(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// sendMessageHandler validates and forwards a message-send request to a specific desktop.
func (s *Server) sendMessageHandler(w http.ResponseWriter, r *http.Request) {
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
		http.Error(w, "empty message", http.StatusBadRequest)
		return
	}

	if len(content) > 8192 {
		http.Error(w, "message too long", http.StatusBadRequest)
		return
	}

	if err := s.Desktops.SendMessage(
		r.Context(),
		desktopID,
		content,
	); err != nil {
		sendMessageError(w, err)
		return
	}

	// Recorded only once the desktop has actually accepted it, so the chat
	// never shows a message that was never delivered. Every human-to-desktop
	// path routes through here — the chat page, the messages page, and a
	// reply to a help request — so all three show up in the transcript.
	s.Chat.AppendUserMessage(desktop.SanitizeID(desktopID), content)

	jsonResponse(w, map[string]string{
		"status": "sent",
	})
}

// sendMessageError answers a failed send with the reason a human can act on,
// rather than the blanket 500 every one of these used to get. A desktop whose
// agent has not bound its port yet is the common case and is not an internal
// error: it is a "not ready yet, try again" the chat can show as-is.
func sendMessageError(w http.ResponseWriter, err error) {
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

	log.Println("sendMessage:", err)
}
