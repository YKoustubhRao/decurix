// Package httpapi implements the Flowera control-plane HTTP API and
// dashboard: it is the only package that touches net/http request/response
// objects, composing the desktop, helpnotes, memory, conversation,
// stepimages, tasks, tunnel, and webui packages.
package httpapi

import (
	"net/http"

	"github.com/YKoustubhRao/flowera/internal/conversation"
	"github.com/YKoustubhRao/flowera/internal/desktop"
	"github.com/YKoustubhRao/flowera/internal/helpnotes"
	"github.com/YKoustubhRao/flowera/internal/memory"
	"github.com/YKoustubhRao/flowera/internal/stepimages"
	"github.com/YKoustubhRao/flowera/internal/tasks"
	"github.com/YKoustubhRao/flowera/internal/tunnel"
	"github.com/YKoustubhRao/flowera/internal/webui"
)

// Server wires the desktop manager, help-notes store, memory store, chat
// store, action-step screenshot store, task records, tunnel store, and web UI
// renderer together to serve the Flowera control plane.
type Server struct {
	Desktops   *desktop.Manager
	HelpNotes  *helpnotes.Store
	Memory     *memory.Store
	Chat       *conversation.Store
	StepImages *stepimages.Store
	Tasks      *tasks.Store
	Tunnel     *tunnel.Store
	Web        *webui.Renderer
}

// NewServer creates a Server from its component stores/managers.
func NewServer(desktops *desktop.Manager, helpNotes *helpnotes.Store, memoryStore *memory.Store, chat *conversation.Store, stepImages *stepimages.Store, taskRecords *tasks.Store, tunnelStore *tunnel.Store, web *webui.Renderer) *Server {
	return &Server{
		Desktops:   desktops,
		HelpNotes:  helpNotes,
		Memory:     memoryStore,
		Chat:       chat,
		StepImages: stepImages,
		Tasks:      taskRecords,
		Tunnel:     tunnelStore,
		Web:        web,
	}
}

// RegisterRoutes registers every HTTP route the control plane serves.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/", s.dashboardHandler)
	mux.HandleFunc("/view/", s.desktopViewHandler)
	mux.HandleFunc("/desktop/", s.desktopProxyHandler)
	mux.HandleFunc("/agent/", s.agentProxyHandler)
	mux.HandleFunc("/api/desktops", s.desktopsAPIHandler)
	mux.HandleFunc("/api/desktops/create", s.createDesktopHandler)
	mux.HandleFunc("/api/desktops/start", s.startDesktopHandler)
	mux.HandleFunc("/api/desktops/stop", s.stopDesktopHandler)
	mux.HandleFunc("/api/desktops/restart", s.restartDesktopHandler)
	mux.HandleFunc("/api/desktops/delete", s.deleteDesktopHandler)
	mux.HandleFunc("/messages", s.messagesDashboardHandler)
	mux.HandleFunc("/api/desktops/send", s.sendMessageHandler)
	mux.HandleFunc("/api/desktops/memory", s.desktopMemoryHandler)
	mux.HandleFunc("/api/desktops/screenshot", s.desktopScreenshotHandler)
	mux.HandleFunc("/api/desktops/chat", s.chatHandler)
	mux.HandleFunc("/api/desktops/chat/stream", s.chatStreamHandler)
	mux.HandleFunc("/api/desktops/chat/clear", s.clearChatHandler)
	mux.HandleFunc(stepImagePath, s.stepImageHandler)
	mux.HandleFunc("/api/agent/events", s.agentEventHandler)
	mux.HandleFunc("/api/helptext", s.helpTextHandler)
	mux.HandleFunc("/api/helpnotes", s.helpNotesHandler)
	mux.HandleFunc("/api/helpnotes/close", s.closeHelpNoteHandler)
	mux.HandleFunc("/dashboard", s.dashboardPostsHandler)
	mux.HandleFunc("/tasks", s.tasksPageHandler)
	mux.HandleFunc("/tasks/", s.taskPageHandler)
	mux.HandleFunc("/api/tasks", s.tasksAPIHandler)
	mux.HandleFunc("/api/tasks/get", s.taskAPIHandler)
	mux.HandleFunc("/api/tasks/report", s.taskReportAPIHandler)
	mux.HandleFunc("/api/tasks/verdict", s.taskVerdictHandler)
	mux.HandleFunc("/recordings/", s.recordingsPageHandler)
	mux.HandleFunc("/api/desktops/recordings", s.recordingsAPIHandler)
	mux.HandleFunc("/api/desktops/recordings/get", s.recordingAPIHandler)
	mux.HandleFunc("/api/desktops/recordings/frame", s.recordingFrameHandler)
	mux.HandleFunc("/api/desktops/recordings/export", s.recordingExportHandler)
	mux.HandleFunc("/api/desktops/recordings/update", s.recordingUpdateHandler)
	mux.HandleFunc("/api/desktops/recordings/delete", s.recordingDeleteHandler)
	mux.HandleFunc("/api/desktops/recordings/replay", s.replayRecordingHandler)
	mux.HandleFunc("/api/desktops/demonstration", s.demonstrationHandler)
	mux.HandleFunc("/api/desktops/record", s.recordTaskHandler)
	mux.HandleFunc("/api/desktops/ticket", s.ticketStatusHandler)
	mux.HandleFunc("/api/desktops/ticket/action", s.ticketActionHandler)
	mux.HandleFunc("/api/tunnel", s.tunnelHandler)
	mux.HandleFunc("/favicon.svg", s.faviconHandler)
}
