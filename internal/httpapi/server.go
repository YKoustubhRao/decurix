// Package httpapi implements the Decurix control-plane HTTP API and
// dashboard: it is the only package that touches net/http request/response
// objects, composing the desktop, querynotes, memory, conversation,
// stepimages, tasks, tunnel, suggest, and webui packages.
package httpapi

import (
	"github.com/YKoustubhRao/decurix/internal/concierge"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/YKoustubhRao/decurix/internal/claudeauth"
	"github.com/YKoustubhRao/decurix/internal/conversation"
	"github.com/YKoustubhRao/decurix/internal/dag"
	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/idlepause"
	"github.com/YKoustubhRao/decurix/internal/keepawake"
	"github.com/YKoustubhRao/decurix/internal/lanaccess"
	"github.com/YKoustubhRao/decurix/internal/mailtrigger"
	"github.com/YKoustubhRao/decurix/internal/memory"
	"github.com/YKoustubhRao/decurix/internal/querynotes"
	"github.com/YKoustubhRao/decurix/internal/resourcemon"
	"github.com/YKoustubhRao/decurix/internal/schedule"
	"github.com/YKoustubhRao/decurix/internal/stepimages"
	"github.com/YKoustubhRao/decurix/internal/suggest"
	"github.com/YKoustubhRao/decurix/internal/tasks"
	"github.com/YKoustubhRao/decurix/internal/tunnel"
	"github.com/YKoustubhRao/decurix/internal/webui"
	"github.com/YKoustubhRao/decurix/internal/winhost"
)

// Server wires the desktop manager, query-notes store, memory store, chat
// store, action-step screenshot store, task records, tunnel store, shared
// Claude session store, LAN-access store, scheduled-request store, DAG
// store, and web UI renderer together to serve the Decurix control plane.
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
	// DagRunner is assigned by cmd/decurix/main.go after construction, not
	// through NewServer: it holds this *Server itself as its Submitter
	// (SubmitTracked), so it cannot be built before the Server exists.
	DagRunner *dag.Runner
	// IdleChecker is optionally assigned by cmd/decurix/main.go after construction,
	// allowing task completion events to request fast-idle transitions.
	IdleChecker *idlepause.Checker
	// Mail and MailRunner are the email triggers (internal/mailtrigger);
	// KeepAwake keeps this machine from idle-sleeping while they or a
	// schedule need it (internal/keepawake). All three are assigned by
	// cmd/decurix/main.go after construction, like DagRunner: the runner
	// holds this *Server as its Submitter.
	Mail       *mailtrigger.Store
	MailRunner *mailtrigger.Runner
	KeepAwake  *keepawake.Keeper
	// ResourceMon monitors container resource usage and handles autoscaling alerts.
	ResourceMon *resourcemon.Monitor
	Suggest     *suggest.Store
	Web         *webui.Renderer
	// Concierge is the chat-first page (internal/concierge); assigned by
	// cmd/decurix/main.go after construction, like DagRunner, since its
	// Deps close over this *Server.
	Concierge *concierge.Concierge

	// sends serialises every send to one desktop (sendlock.go).
	sends sendLocks
	// nets caches the Docker networks' address ranges (humansonly.go).
	nets containerNets
	// keepAwakeScriptAt is when a host keep-awake script last asked
	// (UnixNano; mail.go's keepAwakeHandler).
	keepAwakeScriptAt atomic.Int64

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
	// HostGUIDataDir is where the apps kept ready on the host are saved (RestoreHostApps); "" keeps them in memory only.
	HostGUIDataDir string
	keptHostAppsMu sync.Mutex
	keptHostApps   map[string]keptHostApp
}

// NewServer creates a Server from its component stores/managers.
func NewServer(desktops *desktop.Manager, queryNotes *querynotes.Store, memoryStore *memory.Store, chat *conversation.Store, stepImages *stepimages.Store, taskRecords *tasks.Store, tunnelStore *tunnel.Store, claudeAuth *claudeauth.Store, lanAccess *lanaccess.Store, schedules *schedule.Store, dags *dag.Store, suggestions *suggest.Store, web *webui.Renderer) *Server {
	srv := &Server{
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

	// Pre-warm host apps discovery in background so desktop creation and scanning UI are instant. The scan marks apps the
	// host helper cannot safely run as unavailable, while keeping them visible in the complete discovery list.
	desktop.SetHostAppChecker(srv.checkHostApps)
	desktop.SetHostAppWarmer(srv.warmHostGUI)
	desktop.SetHostAppStarter(srv.startHostGUIAppAhead)
	desktop.WarmHostApps() // compile and load now, long before the first desktop exists
	desktop.InitHostAppsScan()
	return srv
}

// RegisterRoutes registers every HTTP route the control plane serves.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/", s.dashboardHandler)
	mux.HandleFunc("/view/", s.desktopViewHandler)
	// The concierge drives every desktop from one chat, approvals included,
	// so it is a person's only - never an agent's (humansonly.go).
	mux.HandleFunc("/chat", s.humansOnly(s.conciergePageHandler))
	mux.HandleFunc("/api/concierge", s.humansOnly(s.conciergeStateHandler))
	mux.HandleFunc("/api/concierge/send", s.humansOnly(s.conciergeSendHandler))
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
	mux.HandleFunc("/api/schedules", s.schedulesHandler)
	mux.HandleFunc("/api/schedules/create", s.createScheduleHandler)
	mux.HandleFunc("/api/schedules/delete", s.deleteScheduleHandler)
	mux.HandleFunc("/api/schedules/toggle", s.toggleScheduleHandler)
	// The email triggers: a person's mailbox, never an agent's to read or
	// change (humansonly.go).
	mux.HandleFunc("/triggers", s.humansOnly(s.triggersPageHandler))
	mux.HandleFunc("/email", s.humansOnly(emailPageMovedHandler))
	mux.HandleFunc("/api/mail", s.humansOnly(s.mailStateHandler))
	mux.HandleFunc("/api/mail/accounts/connect", s.humansOnly(s.connectMailHandler))
	mux.HandleFunc("/api/mail/accounts/password", s.humansOnly(s.mailPasswordHandler))
	mux.HandleFunc("/api/mail/accounts/toggle", s.humansOnly(s.toggleMailHandler))
	mux.HandleFunc("/api/mail/accounts/check", s.humansOnly(s.checkMailHandler))
	mux.HandleFunc("/api/mail/accounts/delete", s.humansOnly(s.deleteMailHandler))
	mux.HandleFunc("/api/mail/rules/create", s.humansOnly(s.createMailRuleHandler))
	mux.HandleFunc("/api/mail/rules/toggle", s.humansOnly(s.toggleMailRuleHandler))
	mux.HandleFunc("/api/mail/rules/delete", s.humansOnly(s.deleteMailRuleHandler))
	mux.HandleFunc("/api/mail/rules/test", s.humansOnly(s.testMailRuleHandler))
	mux.HandleFunc("/api/mail/jobs", s.humansOnly(s.mailJobHandler))
	mux.HandleFunc("/api/keep-awake", s.keepAwakeHandler)
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
	mux.HandleFunc("/api/desktops/activity", s.activityHandler)
	mux.HandleFunc("/api/desktops/chat/clear", s.clearChatHandler)
	mux.HandleFunc(stepImagePath, s.stepImageHandler)
	mux.HandleFunc("/api/agent/events", s.agentEventHandler)
	mux.HandleFunc("/api/querytext", s.queryTextHandler)
	mux.HandleFunc("/api/querynotes", s.queryNotesHandler)
	mux.HandleFunc("/api/querynotes/close", s.closeQueryNoteHandler)
	mux.HandleFunc("/api/querynotes/resolved", s.resolvedQueryNotesHandler)
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
	mux.HandleFunc("/api/desktops/recordings/import", s.recordingImportHandler)
	mux.HandleFunc("/api/desktops/recordings/replay", s.replayRecordingHandler)
	mux.HandleFunc("/api/desktops/recordings/learn", s.learnRecordingHandler)
	mux.HandleFunc("/skills/", s.skillsPageHandler)
	mux.HandleFunc("/api/desktops/skills", s.skillsAPIHandler)
	mux.HandleFunc("/api/desktops/skills/get", s.skillAPIHandler)
	mux.HandleFunc("/api/desktops/skills/run", s.runSkillHandler)
	mux.HandleFunc("/api/desktops/skills/export", s.skillExportHandler)
	mux.HandleFunc("/api/desktops/skills/delete", s.skillDeleteHandler)
	mux.HandleFunc("/api/desktops/demonstration", s.demonstrationHandler)
	mux.HandleFunc("/api/desktops/record", s.recordTaskHandler)
	mux.HandleFunc("/api/desktops/ticket", s.ticketStatusHandler)
	mux.HandleFunc("/api/desktops/ticket/action", s.ticketActionHandler)
	mux.HandleFunc("/api/tunnel", s.tunnelHandler)
	mux.HandleFunc("/api/model", s.modelHandler)
	mux.HandleFunc("/api/claude-auth/status", s.claudeAuthStatusHandler)
	mux.HandleFunc("/api/claude-auth/login/start", s.claudeAuthLoginStartHandler)
	mux.HandleFunc("/api/claude-auth/login/code", s.claudeAuthLoginCodeHandler)
	mux.HandleFunc("/api/claude-auth/login/cancel", s.claudeAuthLoginCancelHandler)
	mux.HandleFunc("/api/personas", s.personasHandler)
	mux.HandleFunc("/api/personas/get", s.personaHandler)
	mux.HandleFunc("/api/personas/create-desktop", s.createPersonaDesktopHandler)
	mux.HandleFunc("/api/teams", s.teamsHandler)
	mux.HandleFunc("/api/teams/get", s.teamHandler)
	mux.HandleFunc("/api/teams/deploy", s.deployTeamHandler)
	mux.HandleFunc("/api/channels/inbound", s.inboundChannelHandler)
	mux.HandleFunc("/api/skills/catalog", s.skillsCatalogHandler)
	mux.HandleFunc("/api/skills/catalog/install", s.installCatalogSkillHandler)
	mux.HandleFunc("/api/desktops/mode", s.workerModeHandler)
	mux.HandleFunc("/api/desktops/shadowing", s.shadowingHandler)
	mux.HandleFunc("/favicon.svg", s.faviconHandler)
	mux.HandleFunc(noVNCPath, s.noVNCHandler)
}

// RegisterLocalRoutes registers the routes that must only ever be reachable
// on the primary listener, never on the LAN/tunnel-facing one a phone
// reaches: the current LAN access code and the button that rotates it, and
// the host bridge (below).
// Serving the code back over the very listener it gates would make it
// pointless the moment someone else on that listener asked for it, so
// cmd/decurix/main.go calls this only for the primary mux.
func (s *Server) RegisterLocalRoutes(mux *http.ServeMux) {
	// Everything that runs something on the host or shows a host program
	// is for a desktop's own launchers, which reach the primary listener
	// over the Docker network. No phone or tunnel visitor needs any of it,
	// so the gated listener never serves it: a leaked or brute-forced
	// access code must not become command execution on the host.
	mux.HandleFunc("/api/host-bridge/exec", s.hostBridgeExecHandler)
	mux.HandleFunc("/api/host-bridge/open", s.hostBridgeOpenHandler)
	mux.HandleFunc("/api/host-bridge/clip", s.hostBridgeClipHandler)
	mux.HandleFunc("/api/host-gui/start", hostGUIDirectOnly(s.hostGUIStartHandler))
	mux.HandleFunc("/api/host-gui/ws", hostGUIDirectOnly(s.hostGUIWSHandler))
	mux.HandleFunc("/api/host-gui/stop", hostGUIDirectOnly(s.hostGUIStopHandler))
	mux.HandleFunc("/host-gui/view", hostGUIDirectOnly(s.hostGUIViewHandler))
	mux.HandleFunc("/api/lan-access/status", s.lanAccessStatusHandler)
	mux.HandleFunc("/api/lan-access/rotate", s.lanAccessRotateHandler)
}
