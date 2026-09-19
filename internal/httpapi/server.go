// Package httpapi implements the Flowera control-plane HTTP API and
// dashboard: it is the only package that touches net/http request/response
// objects, composing the desktop, querynotes, memory, conversation,
// stepimages, tasks, tunnel, suggest, and webui packages.
package httpapi

import (
	"net/http"
	"sync"

	"github.com/YKoustubhRao/flowera/internal/claudeauth"
	"github.com/YKoustubhRao/flowera/internal/conversation"
	"github.com/YKoustubhRao/flowera/internal/dag"
	"github.com/YKoustubhRao/flowera/internal/desktop"
	"github.com/YKoustubhRao/flowera/internal/idlepause"
	"github.com/YKoustubhRao/flowera/internal/lanaccess"
	"github.com/YKoustubhRao/flowera/internal/memory"
	"github.com/YKoustubhRao/flowera/internal/querynotes"
	"github.com/YKoustubhRao/flowera/internal/resourcemon"
	"github.com/YKoustubhRao/flowera/internal/schedule"
	"github.com/YKoustubhRao/flowera/internal/stepimages"
	"github.com/YKoustubhRao/flowera/internal/suggest"
	"github.com/YKoustubhRao/flowera/internal/tasks"
	"github.com/YKoustubhRao/flowera/internal/tunnel"
	"github.com/YKoustubhRao/flowera/internal/webui"
	"github.com/YKoustubhRao/flowera/internal/winhost"
)

// Server wires the desktop manager, query-notes store, memory store, chat
// store, action-step screenshot store, task records, tunnel store, shared
// Claude session store, LAN-access store, scheduled-request store, DAG
// store, and web UI renderer together to serve the Flowera control plane.
type Server struct {
	Desktops   *desktop.Manager
	QueryNotes *querynotes.Store
	Memory     *memory.Store
	Chat       *conversation.Store
	StepImages *stepimages.Store
	Tasks      *tasks.Store
	Tunnel     *tunnel.Store
	ClaudeAuth *claudeauth.Store
	LANAccess  *lanaccess.Store
	Schedules  *schedule.Store
	Dags       *dag.Store
	// DagRunner is assigned by cmd/flowera/main.go after construction, not
	// through NewServer: it holds this *Server itself as its Submitter
	// (SubmitTracked), so it cannot be built before the Server exists.
	DagRunner *dag.Runner
	// IdleChecker is optionally assigned by cmd/flowera/main.go after construction,
	// allowing task completion events to request fast-idle transitions.
	IdleChecker *idlepause.Checker
	// ResourceMon monitors container resource usage and handles autoscaling alerts.
	ResourceMon *resourcemon.Monitor
	Suggest     *suggest.Store
	Web         *webui.Renderer

	stepImagesMu     sync.Mutex
	recentStepImages map[string]string

	// hostGUI runs Windows apps on a private desktop of the host and streams them to a VM; created on first use.
	hostGUIOnce sync.Once
	hostGUIMgr  *winhost.Manager
	// HostGUISecret signs the launch tokens a launcher carries (winhost.Token); main.go loads it from the data
	// directory so tokens survive a restart. hostGUIAuth holds, per running session, the token it was started with.
	HostGUISecret []byte
	hostGUIAuthMu sync.Mutex
	hostGUIAuth   map[string]string
}

// NewServer creates a Server from its component stores/managers.
func NewServer(desktops *desktop.Manager, queryNotes *querynotes.Store, memoryStore *memory.Store, chat *conversation.Store, stepImages *stepimages.Store, taskRecords *tasks.Store, tunnelStore *tunnel.Store, claudeAuth *claudeauth.Store, lanAccess *lanaccess.Store, schedules *schedule.Store, dags *dag.Store, suggestions *suggest.Store, web *webui.Renderer) *Server {
	// Pre-warm host apps discovery in background so desktop creation and scanning UI are instant
	desktop.InitHostAppsScan()

	return &Server{
		Desktops:         desktops,
		QueryNotes:       queryNotes,
		Memory:           memoryStore,
		Chat:             chat,
		StepImages:       stepImages,
		Tasks:            taskRecords,
		Tunnel:           tunnelStore,
		ClaudeAuth:       claudeAuth,
		LANAccess:        lanAccess,
		Schedules:        schedules,
		Dags:             dags,
		Suggest:          suggestions,
		Web:              web,
		recentStepImages: make(map[string]string),
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
	mux.HandleFunc("/api/desktops/status", s.desktopStatusHandler)
	mux.HandleFunc("/api/desktops/start", s.startDesktopHandler)
	mux.HandleFunc("/api/desktops/stop", s.stopDesktopHandler)
	mux.HandleFunc("/api/desktops/pause", s.pauseDesktopHandler)
	mux.HandleFunc("/api/desktops/unpause", s.unpauseDesktopHandler)
	mux.HandleFunc("/api/desktops/restart", s.restartDesktopHandler)
	mux.HandleFunc("/api/desktops/delete", s.deleteDesktopHandler)
	mux.HandleFunc("/api/desktops/resource-alerts", s.resourceAlertsHandler)
	mux.HandleFunc("/api/desktops/resource-increase", s.resourceIncreaseHandler)
	mux.HandleFunc("/api/desktops/resource-dismiss", s.resourceDismissHandler)
	mux.HandleFunc("/api/desktops/resource-report", s.resourceReportHandler)
	mux.HandleFunc("/requests", s.requestsDashboardHandler)
	mux.HandleFunc("/api/desktops/send", s.sendRequestHandler)
	mux.HandleFunc("/api/desktops/suggest", s.suggestDesktopHandler)
	mux.HandleFunc("/api/host-apps/detect", s.detectHostAppsHandler)
	mux.HandleFunc("/api/desktops/host-apps/copy", s.copyHostAppsHandler)
	mux.HandleFunc("/api/host-bridge/exec", s.hostBridgeExecHandler)
	mux.HandleFunc("/api/host-bridge/open", s.hostBridgeOpenHandler)
	mux.HandleFunc("/api/host-bridge/clip", s.hostBridgeClipHandler)
	mux.HandleFunc("/api/host-gui/start", hostGUIDirectOnly(s.hostGUIStartHandler))
	mux.HandleFunc("/api/host-gui/stream", hostGUIDirectOnly(s.hostGUIStreamHandler))
	mux.HandleFunc("/api/host-gui/frame", hostGUIDirectOnly(s.hostGUIFrameHandler))
	mux.HandleFunc("/api/host-gui/input", hostGUIDirectOnly(s.hostGUIInputHandler))
	mux.HandleFunc("/api/host-gui/stop", hostGUIDirectOnly(s.hostGUIStopHandler))
	mux.HandleFunc("/host-gui/view", hostGUIDirectOnly(s.hostGUIViewHandler))
	mux.HandleFunc("/api/schedules", s.schedulesHandler)
	mux.HandleFunc("/api/schedules/create", s.createScheduleHandler)
	mux.HandleFunc("/api/schedules/delete", s.deleteScheduleHandler)
	mux.HandleFunc("/api/schedules/toggle", s.toggleScheduleHandler)
	mux.HandleFunc("/dags", s.dagsPageHandler)
	mux.HandleFunc("/dags/", s.dagPageHandler)
	mux.HandleFunc("/api/dags", s.dagsAPIHandler)
	mux.HandleFunc("/api/dags/create", s.createDagHandler)
	mux.HandleFunc("/api/dags/get", s.dagAPIHandler)
	mux.HandleFunc("/api/dags/delete", s.deleteDagHandler)
	mux.HandleFunc("/api/dags/nodes/create", s.createDagNodeHandler)
	mux.HandleFunc("/api/dags/nodes/update", s.updateDagNodeHandler)
	mux.HandleFunc("/api/dags/nodes/position", s.dagNodePositionHandler)
	mux.HandleFunc("/api/dags/nodes/delete", s.deleteDagNodeHandler)
	mux.HandleFunc("/api/dags/nodes/set-head", s.setDagHeadHandler)
	mux.HandleFunc("/api/dags/edges/create", s.createDagEdgeHandler)
	mux.HandleFunc("/api/dags/edges/delete", s.deleteDagEdgeHandler)
	mux.HandleFunc("/api/dags/cron/set", s.setDagCronHandler)
	mux.HandleFunc("/api/dags/run", s.runDagHandler)
	mux.HandleFunc("/api/dags/runs", s.dagRunsHandler)
	mux.HandleFunc("/api/dags/runs/get", s.dagRunAPIHandler)
	mux.HandleFunc("/api/desktops/memory", s.desktopMemoryHandler)
	mux.HandleFunc("/api/desktops/memory/link", s.linkDesktopMemoryHandler)
	mux.HandleFunc("/api/desktops/memory/unlink", s.unlinkDesktopMemoryHandler)
	mux.HandleFunc("/api/desktops/screenshot", s.desktopScreenshotHandler)
	mux.HandleFunc("/api/desktops/chat", s.chatHandler)
	mux.HandleFunc("/api/desktops/chat/stream", s.chatStreamHandler)
	mux.HandleFunc("/api/desktops/chat/clear", s.clearChatHandler)
	mux.HandleFunc(stepImagePath, s.stepImageHandler)
	mux.HandleFunc("/api/agent/events", s.agentEventHandler)
	mux.HandleFunc("/api/querytext", s.queryTextHandler)
	mux.HandleFunc("/api/querynotes", s.queryNotesHandler)
	mux.HandleFunc("/api/querynotes/close", s.closeQueryNoteHandler)
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
	mux.HandleFunc("/api/desktops/recordings/learn", s.learnRecordingHandler)
	mux.HandleFunc("/skills/", s.skillsPageHandler)
	mux.HandleFunc("/api/desktops/skills", s.skillsAPIHandler)
	mux.HandleFunc("/api/desktops/skills/get", s.skillAPIHandler)
	mux.HandleFunc("/api/desktops/skills/run", s.runSkillHandler)
	mux.HandleFunc("/api/desktops/skills/delete", s.skillDeleteHandler)
	mux.HandleFunc("/api/desktops/demonstration", s.demonstrationHandler)
	mux.HandleFunc("/api/desktops/record", s.recordTaskHandler)
	mux.HandleFunc("/api/desktops/ticket", s.ticketStatusHandler)
	mux.HandleFunc("/api/desktops/ticket/action", s.ticketActionHandler)
	mux.HandleFunc("/api/tunnel", s.tunnelHandler)
	mux.HandleFunc("/api/claude-auth/status", s.claudeAuthStatusHandler)
	mux.HandleFunc("/api/claude-auth/login/start", s.claudeAuthLoginStartHandler)
	mux.HandleFunc("/api/claude-auth/login/code", s.claudeAuthLoginCodeHandler)
	mux.HandleFunc("/api/claude-auth/login/cancel", s.claudeAuthLoginCancelHandler)
	mux.HandleFunc("/favicon.svg", s.faviconHandler)
}

// RegisterLocalRoutes registers the handful of routes that must only ever be
// reachable on the primary listener, never on the LAN/tunnel-facing one a
// phone reaches: the current LAN access code and the button that rotates it.
// Serving the code back over the very listener it gates would make it
// pointless the moment someone else on that listener asked for it, so
// cmd/flowera/main.go calls this only for the primary mux.
func (s *Server) RegisterLocalRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/lan-access/status", s.lanAccessStatusHandler)
	mux.HandleFunc("/api/lan-access/rotate", s.lanAccessRotateHandler)
}
