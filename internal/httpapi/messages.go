package httpapi

import (
	"net/http"
	"strings"

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
		Host:     getHost(r),
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
		jsonError(w, err)
		return
	}

	jsonResponse(w, map[string]string{
		"status": "sent",
	})
}
