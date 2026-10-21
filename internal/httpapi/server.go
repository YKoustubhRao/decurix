// Package httpapi implements the Decurix control-plane HTTP API and
// dashboard: it is the only package that touches net/http request/response
// objects, composing the desktop, querynotes, memory, conversation,
// stepimages, tasks, tunnel, suggest, and webui packages.
package httpapi

import (
	"context"
	"database/sql"
	"github.com/YKoustubhRao/decurix/internal/llm"
	"github.com/YKoustubhRao/decurix/internal/outbox"
	"io"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/YKoustubhRao/decurix/internal/cftunnel"
	"github.com/YKoustubhRao/decurix/internal/claudeauth"
	"github.com/YKoustubhRao/decurix/internal/conversation"
	"github.com/YKoustubhRao/decurix/internal/dag"
	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/devices"
	"github.com/YKoustubhRao/decurix/internal/grounder"
	"github.com/YKoustubhRao/decurix/internal/idlepause"
	"github.com/YKoustubhRao/decurix/internal/keepawake"
	"github.com/YKoustubhRao/decurix/internal/lanaccess"
	"github.com/YKoustubhRao/decurix/internal/mailtrigger"
	"github.com/YKoustubhRao/decurix/internal/memory"
	"github.com/YKoustubhRao/decurix/internal/phonehost"
	"github.com/YKoustubhRao/decurix/internal/qualification"
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
	// finished is the last few tasks that ended done, for the pages' notification (finished.go).
	finished finishedFeed

	Desktops   *desktop.Manager
	QueryNotes *querynotes.Store
	Memory     *memory.Store
	Chat       *conversation.Store
	StepImages *stepimages.Store
	Tasks      *tasks.Store
	// Qualification owns evidence and grants; desktop agents cannot write them.
	Qualification     *qualification.Store
	qualificationAuth qualificationAuth
	qualificationRuns qualificationRuns
	Tunnel            *tunnel.Store
	ClaudeAuth        *claudeauth.Store
	LANAccess         *lanaccess.Store
	// Devices is the phones and laptops paired with the gated listener
	// (internal/devices), listed and revoked from the Mobile menu. Assigned
	// by cmd/decurix/main.go after construction; nil means pairing is off.
	Devices *devices.Store
	// CFTunnel is the named Cloudflare tunnel: a fixed link on the
	// person's own domain for the gated listener (internal/cftunnel).
	// Assigned by cmd/decurix/main.go; nil means it is not available.
	CFTunnel *cftunnel.Manager
	// Phones is every Android phone linked right now (internal/phonehost);
	// set with SetPhones, nil means phone links are off.
	Phones *phonehost.Hub
	// phoneSetup holds the one-time tokens of phone setups in progress.
	phoneSetup phoneSetups
	// termux caches where the latest Termux installer is.
	termux    termuxLatest
	Schedules *schedule.Store
	Dags      *dag.Store
	// DagRunner is assigned by cmd/decurix/main.go after construction, not
	// through NewServer: it holds this *Server itself as its Submitter
	// (SubmitTracked), so it cannot be built before the Server exists.
	DagRunner *dag.Runner
	// IdleChecker is optionally assigned by cmd/decurix/main.go after construction,
	// allowing task completion events to request fast-idle transitions.
	IdleChecker *idlepause.Checker
	// Grounder is the GUI-grounding model (internal/grounder), set from
	// .env by cmd/decurix/main.go; nil means none is set up.
	Grounder *grounder.Client
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
	// LLM holds the hosted models a person set up and forwards the desktops'
	// chats to them (internal/llm). main.go replaces the in-memory default
	// with one saved in the database.
	// NewServer reads it from the environment; unconfigured, its routes say so.
	LLM *llm.Hub
	// Outbox holds the emails agents drafted for a person to send
	// (internal/outbox). Assigned by cmd/decurix/main.go; nil turns it off.
	Outbox *outbox.Store
	// DB is the control plane's database, read only for a backup's snapshot
	// (backup.go). Assigned by cmd/decurix/main.go; nil turns backups off.
	DB *sql.DB

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
	// keptHostAppsLive are the desktops whose kept apps run on the host now: only a running (or paused) desktop's do
	// (syncHostApps). keptHostApps itself is kept across a stop, so the desktop's next start keeps them again.
	keptHostAppsLive map[string]bool
	// hostAppDesktops lists the desktops for syncHostApps; nil means Desktops.List (tests set it).
	hostAppDesktops func(ctx context.Context) ([]desktop.Desktop, error)
	// hostVision is each host app's vision fallback for reading its controls (hostcontrols.go).
	hostVisionMu sync.Mutex
	hostVision   map[string]*hostVision

	// agentRequest stands in for Desktops.AgentRequest there, in tests.
	agentRequest func(ctx context.Context, id, method, path string, body io.Reader) (*http.Response, error)
}

// mustMemoryHub is an llm.Hub kept in memory only - NewHub cannot fail
// without a database.
func mustMemoryHub() *llm.Hub {
	hub, err := llm.NewHub(nil, nil)
	if err != nil {
		panic(err)
	}
	return hub
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
		LLM:              mustMemoryHub(),
		recentStepImages: make(map[string]string),
	}

	// Pre-warm host apps discovery in background so desktop creation and scanning UI are instant. The scan marks apps the
	// host helper cannot safely run as unavailable, while keeping them visible in the complete discovery list.
	desktop.SetHostAppChecker(srv.checkHostApps)
	desktop.SetHostAppWarmer(srv.warmHostGUI)
	desktop.SetHostAppStarter(srv.startHostGUIAppAhead)
	desktop.SetHostAppPageFinder(func(target, args string) string { return srv.hostGUI().LocalPage(target, args) })
	desktop.WarmHostApps() // compile and load now, long before the first desktop exists
	desktop.InitHostAppsScan()
	return srv
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
	mux.HandleFunc("/api/host-paths/validate", s.validateHostPathHandler)
	// A linked phone connects here as a paired device (the gate checks its
	// cookie on the tunnel's listener, the handler checks it on both).
	mux.HandleFunc("/api/phones/connect", s.phoneConnectHandler)
	mux.HandleFunc("/api/phones", s.phonesHandler)
	// Linking a phone from the phone: the page, its token, and the three
	// paths Termux fetches with that token (let past the gate, see
	// PhoneSetupOpenPaths).
	mux.HandleFunc("/phone/link", s.phoneLinkPageHandler)
	mux.HandleFunc("/phone/termux.apk", s.phoneTermuxHandler)
	mux.HandleFunc("/api/phones/setup-token", s.phoneSetupTokenHandler)
	mux.HandleFunc("/phone/setup.sh", s.phoneSetupScriptHandler)
	mux.HandleFunc("/phone/decurix", s.phoneBinaryHandler)
	mux.HandleFunc("/api/phones/claim", s.phoneClaimHandler)
	mux.HandleFunc("/api/desktops/host-apps/copy", s.copyHostAppsHandler)
	mux.HandleFunc("/api/schedules", s.schedulesHandler)
	mux.HandleFunc("/api/schedules/create", s.createScheduleHandler)
	mux.HandleFunc("/api/schedules/delete", s.deleteScheduleHandler)
	mux.HandleFunc("/api/schedules/toggle", s.toggleScheduleHandler)
	// The email triggers: a person's mailbox, never an agent's to read or
	// change (humansonly.go).
	mux.HandleFunc("/triggers", s.humansOnly(s.triggersPageHandler))
	mux.HandleFunc("/email", s.humansOnly(emailPageMovedHandler))
	// Whether a hosted model is configured (no key): the create dialog reads
	// it on either listener. Chatting with it is primary-only (RegisterLocalRoutes).
	mux.HandleFunc("/api/llm", s.llmStatusHandler)
	mux.HandleFunc("/api/outbox/list", s.humansOnly(s.outboxListHandler))
	mux.HandleFunc("/api/outbox/file", s.humansOnly(s.outboxFileHandler))
	mux.HandleFunc("/api/outbox/send", s.humansOnly(s.outboxSendHandler))
	mux.HandleFunc("/api/outbox/discard", s.humansOnly(s.outboxDiscardHandler))
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
	mux.HandleFunc("/api/tasks/finished", s.finishedTasksHandler)
	mux.HandleFunc("/api/querynotes/close", s.closeQueryNoteHandler)
	mux.HandleFunc("/api/querynotes/resolved", s.resolvedQueryNotesHandler)
	mux.HandleFunc("/dashboard", s.dashboardPostsHandler)
	mux.HandleFunc("/tasks", s.tasksPageHandler)
	mux.HandleFunc("/tasks/", s.taskPageHandler)
	mux.HandleFunc("/api/tasks", s.tasksAPIHandler)
	mux.HandleFunc("/api/tasks/get", s.taskAPIHandler)
	mux.HandleFunc("/api/tasks/report", s.taskReportAPIHandler)
	mux.HandleFunc("/api/tasks/verdict", s.taskVerdictHandler)
	// The whole record as a file: a person's, never a desktop's to read.
	mux.HandleFunc("/api/tasks/export", s.humansOnly(s.taskExportHandler))
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
	mux.HandleFunc("/api/desktops/workflows", s.workflowsAPIHandler)
	mux.HandleFunc("/api/desktops/skills/get", s.skillAPIHandler)
	mux.HandleFunc("/api/desktops/skills/run", s.runSkillHandler)
	mux.HandleFunc("/api/desktops/skills/export", s.skillExportHandler)
	mux.HandleFunc("/api/desktops/skills/delete", s.skillDeleteHandler)
	mux.HandleFunc("/api/desktops/playbooks", s.playbooksAPIHandler)
	mux.HandleFunc("/api/desktops/playbooks/get", s.playbookAPIHandler)
	mux.HandleFunc("/api/desktops/playbooks/delete", s.playbookDeleteHandler)
	mux.HandleFunc("/api/desktops/demonstration", s.demonstrationHandler)
	mux.HandleFunc("/api/desktops/record", s.recordTaskHandler)
	mux.HandleFunc("/api/desktops/ticket", s.ticketStatusHandler)
	mux.HandleFunc("/api/desktops/ticket/action", s.ticketActionHandler)
	mux.HandleFunc("/api/desktops/ticket/attach", s.ticketAttachHandler)
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
	// Owner-session protected, primary only; never delegated to desktop agents.
	mux.HandleFunc("/readiness/", s.readinessPageHandler)
	mux.HandleFunc("/api/readiness/session", s.qualificationSessionHandler)
	mux.HandleFunc("/api/readiness/list", s.qualificationOwner(s.qualificationListHandler))
	mux.HandleFunc("/api/readiness/source", s.qualificationOwner(s.qualificationSourceHandler))
	mux.HandleFunc("/api/readiness/define", s.qualificationOwner(s.qualificationDefineHandler))
	mux.HandleFunc("/api/readiness/authorize", s.qualificationOwner(s.qualificationAuthorizeHandler))
	mux.HandleFunc("/api/readiness/run", s.qualificationOwner(s.qualificationRunHandler))
	mux.HandleFunc("/api/readiness/cancel", s.qualificationOwner(s.qualificationCancelHandler))
	mux.HandleFunc("/api/readiness/evidence", s.qualificationOwner(s.qualificationEvidenceHandler))
	mux.HandleFunc("/api/readiness/history", s.qualificationOwner(s.qualificationHistoryHandler))
	mux.HandleFunc("/api/readiness/artifact", s.qualificationOwner(s.qualificationArtifactHandler))
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
	// A phone app is shown the way a host app is: in a window of the VM,
	// reaching this listener directly with a token signed for that app.
	mux.HandleFunc("/phone-app/view", hostGUIDirectOnly(s.phoneAppViewHandler))
	mux.HandleFunc("/api/phone-app/ws", hostGUIDirectOnly(s.phoneAppWSHandler))
	// The file chooser opens on this computer's own screen: for the person
	// sitting at it, never a phone or a desktop's agent.
	mux.HandleFunc("/api/host-paths/browse", s.humansOnly(s.browseHostPathHandler))
	// Whether a file a task was asked to save on this computer is there - for the check of that task.
	mux.HandleFunc("/api/host-files/stat", hostGUIDirectOnly(s.hostFileStatHandler))
	mux.HandleFunc("/api/host-files/release", hostGUIDirectOnly(s.hostFileReleaseHandler))
	mux.HandleFunc("/api/host-files/read", hostGUIDirectOnly(s.hostFileReadHandler))
	mux.HandleFunc("/api/lan-access/status", s.lanAccessStatusHandler)
	mux.HandleFunc("/api/lan-access/rotate", s.lanAccessRotateHandler)
	mux.HandleFunc("/api/lan-access/devices", s.lanAccessDevicesHandler)
	mux.HandleFunc("/api/lan-access/devices/revoke", s.lanAccessRevokeDeviceHandler)
	// The Cloudflare API token and the tunnel it controls are the person's
	// alone: never on the gated listener, never to a desktop.
	// A backup holds the keys to the saved secrets: a person's, here only.
	mux.HandleFunc("/api/backup", s.humansOnly(s.backupHandler))
	mux.HandleFunc("/api/desktops/restore", s.humansOnly(s.restoreDesktopHandler))
	mux.HandleFunc("/api/cftunnel", s.humansOnly(s.cfTunnelStatusHandler))
	mux.HandleFunc("/api/cftunnel/configure", s.humansOnly(s.cfTunnelConfigureHandler))
	mux.HandleFunc("/api/cftunnel/disable", s.humansOnly(s.cfTunnelDisableHandler))
	// The hosted models: set up by a person (their keys never leave this
	// listener), used by the desktops' agents over the Docker network.
	mux.HandleFunc("/api/llm/settings", s.humansOnly(s.llmSettingsHandler))
	mux.HandleFunc("/api/llm/settings/forget", s.humansOnly(s.llmForgetHandler))
	mux.HandleFunc("/api/llm/", s.llmProxyHandler)
	// Where something is on a screenshot, for a desktop's `gui` (grounder.go).
	mux.HandleFunc("/api/grounder", s.grounderStatusHandler)
	mux.HandleFunc("/api/grounder/locate", s.grounderLocateHandler)
	mux.HandleFunc("/api/native-ollama/", s.nativeOllamaHandler)
	// Drafts from the agents' draft_email; sending them is humansOnly above.
	mux.HandleFunc("/api/outbox", s.outboxCreateHandler)
}
