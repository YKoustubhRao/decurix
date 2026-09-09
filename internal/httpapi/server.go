// Package httpapi implements the Flowera control-plane HTTP API and
// dashboard: it is the only package that touches net/http request/response
// objects, composing the desktop, helpnotes, memory, and webui packages.
package httpapi

import (
	"net/http"

	"github.com/YKoustubhRao/flowera/internal/desktop"
	"github.com/YKoustubhRao/flowera/internal/helpnotes"
	"github.com/YKoustubhRao/flowera/internal/memory"
	"github.com/YKoustubhRao/flowera/internal/tunnel"
	"github.com/YKoustubhRao/flowera/internal/webui"
)

// Server wires the desktop manager, help-notes store, memory store, tunnel
// store, and web UI renderer together to serve the Flowera control plane.
type Server struct {
	Desktops  *desktop.Manager
	HelpNotes *helpnotes.Store
	Memory    *memory.Store
	Tunnel    *tunnel.Store
	Web       *webui.Renderer
}

// NewServer creates a Server from its component stores/managers.
func NewServer(desktops *desktop.Manager, helpNotes *helpnotes.Store, memoryStore *memory.Store, tunnelStore *tunnel.Store, web *webui.Renderer) *Server {
	return &Server{
		Desktops:  desktops,
		HelpNotes: helpNotes,
		Memory:    memoryStore,
		Tunnel:    tunnelStore,
		Web:       web,
	}
}

// RegisterRoutes registers every HTTP route the control plane serves.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/", s.dashboardHandler)
	mux.HandleFunc("/desktop/", s.desktopProxyHandler)
	mux.HandleFunc("/api/desktops", s.desktopsAPIHandler)
	mux.HandleFunc("/api/desktops/create", s.createDesktopHandler)
	mux.HandleFunc("/api/desktops/start", s.startDesktopHandler)
	mux.HandleFunc("/api/desktops/stop", s.stopDesktopHandler)
	mux.HandleFunc("/api/desktops/restart", s.restartDesktopHandler)
	mux.HandleFunc("/api/desktops/delete", s.deleteDesktopHandler)
	mux.HandleFunc("/messages", s.messagesDashboardHandler)
	mux.HandleFunc("/api/desktops/send", s.sendMessageHandler)
	mux.HandleFunc("/api/desktops/memory", s.desktopMemoryHandler)
	mux.HandleFunc("/api/helptext", s.helpTextHandler)
	mux.HandleFunc("/api/helpnotes", s.helpNotesHandler)
	mux.HandleFunc("/api/helpnotes/close", s.closeHelpNoteHandler)
	mux.HandleFunc("/dashboard", s.dashboardPostsHandler)
	mux.HandleFunc("/api/tunnel", s.tunnelHandler)
}
