// Package webui renders the HTML dashboard/requests pages.
package webui

import (
	"html/template"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/dag"
	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/memory"
	"github.com/YKoustubhRao/decurix/internal/querynotes"
	"github.com/YKoustubhRao/decurix/internal/tasks"
	"github.com/YKoustubhRao/decurix/web"
)

// templatesDir is the directory inside web.TemplatesFS, not a host path — the
// templates are compiled into the binary, so the executable is self-contained.
const templatesDir = "templates"

// themeFile holds the design tokens, icon sprite and shared chrome markup/JS
// that every page pulls in with {{template "theme"}} and friends. It has to be
// parsed alongside each page template for those names to resolve.
const themeFile = "theme.html"

// templatePaths returns the embedded file list to parse for a page: the page
// itself first (so the parsed template takes its name) plus the shared theme.
func templatePaths(page string, partials ...string) []string {
	paths := []string{
		path.Join(templatesDir, page),
		path.Join(templatesDir, themeFile),
	}
	for _, partial := range partials {
		paths = append(paths, path.Join(templatesDir, partial))
	}
	return paths
}

// triggerParts holds what the requests and triggers pages share for setting
// up a trigger: the email-rule fields and the script that fills and reads them.
const triggerParts = "triggerparts.html"

// PageData is the template data passed to the dashboard page: the current desktops, the worker memory, and the current cloud tunnel URL (if any).
type PageData struct {
	DefaultDesktopCPUs     int64
	DefaultDesktopMemoryMB int64
	Desktops               []desktop.Desktop
	WorkerMemory           []memory.WorkerMemoryEntry
	MemoryByDesktop        map[string][]memory.WorkerMemoryEntry
	// LinkedMemory maps a desktop ID to the other desktop IDs it shares L1
	// memory with (see memory.Store.Link) - what the card's "Shared with"
	// row and unlink button read.
	LinkedMemory map[string][]string
	// DesktopNames maps a desktop ID to its display name, so a card can show
	// a linked/parent desktop's name (matching how the rest of the card
	// always shows .Name, never a raw ID) rather than the ID itself. A
	// lookup miss (the desktop was since deleted) falls back to the ID in
	// the template.
	DesktopNames map[string]string
	TunnelURL    string
}

// QueryNotesPageData is the template data for the query-notes dashboard.
type QueryNotesPageData struct {
	Notes []querynotes.QueryNote
}

// DesktopViewPageData is the template data for a single desktop's detail
// page: its Desktop/Agent tab switcher and split view.
type DesktopViewPageData struct {
	Desktop desktop.Desktop
}

// WindowOption is one report period the task dashboard offers.
type WindowOption struct {
	Value  string
	Label  string
	Phrase string
	Active bool
}

// TasksPageData is the template data for the task records dashboard.
type TasksPageData struct {
	Window  WindowOption
	Windows []WindowOption
	Report  tasks.Report
	Tasks   []tasks.Summary
	Filter  tasks.Filter
}

// TaskPageData is the template data for one task record.
type TaskPageData struct {
	Task tasks.Detail
}

// DagsPageData is the template data for the DAGs list page.
type DagsPageData struct {
	Dags []dag.Summary
}

// DagPageData is the template data for one DAG's editor page. Only the DAG
// record itself - its name and cron expression - is rendered server-side;
// the canvas and run history are populated by the page's own JS fetches
// (loadDag/loadRuns), so the handler has no reason to pay for the nodes,
// edges and run-history queries dag.Detail would otherwise do on every page
// view just to have them go unused.
type DagPageData struct {
	DAG      dag.DAG
	Desktops []desktop.Desktop
}

// RecordingsPageData is the template data for one desktop's recordings page.
// The recordings are not in it: the page reads them from the desktop, which
// keeps them. Session is the recording to open first, if any.
type RecordingsPageData struct {
	Desktop desktop.Desktop
	Session string
}

// SkillsPageData is the template data for one desktop's skills page. The
// skills are not in it: the page reads them from the desktop, which keeps them.
type SkillsPageData struct {
	Desktop desktop.Desktop
}

// taskFuncs are the formatting helpers the task pages need, which are about
// presenting records rather than anything a page should compute itself.
var taskFuncs = template.FuncMap{
	"engineLabel": tasks.EngineLabel,
	"typeLabel":   tasks.TypeLabel,
	"statusLabel": tasks.StatusLabel,
	"contains":    strings.Contains,
	"reasonLabel": tasks.ReasonLabel,
	"duration":    tasks.FormatDuration,
	"cost":        tasks.FormatCost,
	"isApproval":  tasks.IsApprovalReason,
	"since": func(from, to time.Time) time.Duration {
		if from.IsZero() || to.IsZero() {
			return 0
		}
		return to.Sub(from)
	},
	"iso": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.Format(time.RFC3339)
	},
	"clock": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.Format("02 Jan, 15:04")
	},
	"add": func(a, b int) int { return a + b },
	"now": time.Now,
	"percent": func(part, whole int) int {
		if whole <= 0 {
			return 0
		}
		return int(float64(part)*100/float64(whole) + 0.5)
	},
	"deref": func(value *int64) int64 {
		if value == nil {
			return 0
		}
		return *value
	},
	"live": func(status string) bool {
		return status == tasks.StatusQueued || status == tasks.StatusRunning || status == tasks.StatusWaiting
	},
}

// Renderer loads and executes the web UI's HTML templates.
type Renderer struct {
	indexTmpl      *template.Template
	requestsTmpl   *template.Template
	dashboardTmpl  *template.Template
	desktopTmpl    *template.Template
	tasksTmpl      *template.Template
	taskTmpl       *template.Template
	recordingsTmpl *template.Template
	skillsTmpl     *template.Template
	dagsTmpl       *template.Template
	dagTmpl        *template.Template
	hostGUITmpl    *template.Template
	phoneAppTmpl   *template.Template
	phoneLinkTmpl  *template.Template
	screenTmpl     *template.Template
	triggersTmpl   *template.Template
}

// NewRenderer parses all templates once at startup.
func NewRenderer() *Renderer {
	return &Renderer{
		indexTmpl: template.Must(
			template.ParseFS(web.TemplatesFS, templatePaths("index.html")...),
		),
		requestsTmpl: template.Must(
			template.ParseFS(web.TemplatesFS, templatePaths("requests.html", triggerParts)...),
		),
		dashboardTmpl: template.Must(
			template.ParseFS(web.TemplatesFS, templatePaths("dashboard.html")...),
		),
		desktopTmpl: template.Must(
			template.ParseFS(web.TemplatesFS, templatePaths("desktop.html")...),
		),
		tasksTmpl: template.Must(
			template.New("tasks.html").Funcs(taskFuncs).ParseFS(web.TemplatesFS, templatePaths("tasks.html")...),
		),
		taskTmpl: template.Must(
			template.New("task.html").Funcs(taskFuncs).ParseFS(web.TemplatesFS, templatePaths("task.html")...),
		),
		recordingsTmpl: template.Must(
			template.ParseFS(web.TemplatesFS, templatePaths("recordings.html")...),
		),
		skillsTmpl: template.Must(
			template.ParseFS(web.TemplatesFS, templatePaths("skills.html")...),
		),
		dagsTmpl: template.Must(
			template.New("dags.html").Funcs(taskFuncs).ParseFS(web.TemplatesFS, templatePaths("dags.html")...),
		),
		dagTmpl: template.Must(
			template.New("dag.html").Funcs(taskFuncs).ParseFS(web.TemplatesFS, templatePaths("dag.html")...),
		),
		hostGUITmpl: template.Must(
			template.ParseFS(web.TemplatesFS, templatePaths("hostgui.html")...),
		),
		phoneAppTmpl: template.Must(
			template.ParseFS(web.TemplatesFS, "templates/phoneapp.html"),
		),
		phoneLinkTmpl: template.Must(
			template.ParseFS(web.TemplatesFS, "templates/phonelink.html"),
		),
		screenTmpl: template.Must(
			template.ParseFS(web.TemplatesFS, templatePaths("screen.html")...),
		),
		triggersTmpl: template.Must(
			template.ParseFS(web.TemplatesFS, templatePaths("triggers.html", triggerParts)...),
		),
	}
}

// RenderIndex renders the root dashboard page.
func (rd *Renderer) RenderIndex(w io.Writer, data PageData) error {
	data.DefaultDesktopCPUs = 1
	data.DefaultDesktopMemoryMB = 2048
	if n, err := strconv.ParseInt(os.Getenv("DECURIX_DEFAULT_DESKTOP_CPUS"), 10, 64); err == nil && n > 0 && n <= 64 {
		data.DefaultDesktopCPUs = n
	}
	if n, err := strconv.ParseInt(os.Getenv("DECURIX_DEFAULT_DESKTOP_MEMORY_MB"), 10, 64); err == nil && n > 0 && n <= 65536 {
		data.DefaultDesktopMemoryMB = n
	}
	return rd.indexTmpl.ExecuteTemplate(w, "index.html", data)
}

// RenderRequests renders the requests page.
func (rd *Renderer) RenderRequests(w io.Writer, data PageData) error {
	return rd.requestsTmpl.ExecuteTemplate(w, "requests.html", data)
}

// RenderTriggers renders the triggers page: schedules, one-time runs and
// email triggers, the ways a request starts without a person sending it.
func (rd *Renderer) RenderTriggers(w io.Writer, data PageData) error {
	return rd.triggersTmpl.ExecuteTemplate(w, "triggers.html", data)
}

// RenderQueryNotesDashboard renders the query-notes dashboard.
func (rd *Renderer) RenderQueryNotesDashboard(w io.Writer, data QueryNotesPageData) error {
	return rd.dashboardTmpl.ExecuteTemplate(w, "dashboard.html", data)
}

// RenderDesktopView renders a single desktop's detail page (Desktop/Agent
// tabs, split view).
func (rd *Renderer) RenderDesktopView(w io.Writer, data DesktopViewPageData) error {
	return rd.desktopTmpl.ExecuteTemplate(w, "desktop.html", data)
}

// RenderTasks renders the task records dashboard.
func (rd *Renderer) RenderTasks(w io.Writer, data TasksPageData) error {
	return rd.tasksTmpl.ExecuteTemplate(w, "tasks.html", data)
}

// RenderTask renders one task record.
func (rd *Renderer) RenderTask(w io.Writer, data TaskPageData) error {
	return rd.taskTmpl.ExecuteTemplate(w, "task.html", data)
}

// RenderRecordings renders one desktop's recordings page.
func (rd *Renderer) RenderRecordings(w io.Writer, data RecordingsPageData) error {
	return rd.recordingsTmpl.ExecuteTemplate(w, "recordings.html", data)
}

// RenderSkills renders one desktop's skills page.
func (rd *Renderer) RenderSkills(w io.Writer, data SkillsPageData) error {
	return rd.skillsTmpl.ExecuteTemplate(w, "skills.html", data)
}

// RenderDags renders the DAGs list page.
func (rd *Renderer) RenderDags(w io.Writer, data DagsPageData) error {
	return rd.dagsTmpl.ExecuteTemplate(w, "dags.html", data)
}

// RenderDag renders one DAG's editor page.
func (rd *Renderer) RenderDag(w io.Writer, data DagPageData) error {
	return rd.dagTmpl.ExecuteTemplate(w, "dag.html", data)
}

// HostGUIPageData is the template data for the window a desktop opens to show and drive an app running on the host.
type HostGUIPageData struct {
	// Token is the launch token signed for this app; the viewer presents it on every request it makes.
	Token string
	ID    string
	// Desktop is the VM showing this window, so it can be kept from idle-pausing while the window is open.
	Desktop string
	Title   string
	Target  string
	Args    string
}

// PhoneAppPageData is the template data for the window a desktop opens to show and drive an app on a linked phone.
type PhoneAppPageData struct {
	// Token is the launch token signed for this app on this phone.
	Token string
	ID    string
	// Desktop is the VM showing the window, kept from idle-pausing while it is open.
	Desktop string
	Title   string
	// Device is the paired phone, Package the app on it.
	Device  string
	Package string
}

// PhoneLinkPageData is the template data for the page that sets up a phone link from the phone itself.
type PhoneLinkPageData struct {
	// DeviceID and Name are the paired device opening the page ("" when it is not one).
	DeviceID string
	Name     string
	// BinaryReady says this control plane has the phone build to hand out.
	BinaryReady bool
	// Enabled says phone links are on at all.
	Enabled bool
}

// RenderPhoneLink renders the phone link setup page.
func (rd *Renderer) RenderPhoneLink(w io.Writer, data PhoneLinkPageData) error {
	return rd.phoneLinkTmpl.ExecuteTemplate(w, "phonelink.html", data)
}

// RenderPhoneApp renders the viewer for one app running on a linked phone.
func (rd *Renderer) RenderPhoneApp(w io.Writer, data PhoneAppPageData) error {
	return rd.phoneAppTmpl.ExecuteTemplate(w, "phoneapp.html", data)
}

// RenderHostGUI renders the viewer for one app running on a private host desktop.
func (rd *Renderer) RenderHostGUI(w io.Writer, data HostGUIPageData) error {
	return rd.hostGUITmpl.ExecuteTemplate(w, "hostgui.html", data)
}

// ScreenPageData is the template data for a desktop's screen viewer (/desktop/<id>/).
type ScreenPageData struct {
	ID   string
	Name string
	// Password is the desktop's VNC password (desktop.ScreenAccess), put into
	// the page's script, never its URL.
	Password string
	// NoVNC is the path the RFB client module is served at.
	NoVNC string
	// Controls is the recording ticket toolbar and resource alert markup the
	// control plane already injected into noVNC's own page (httpapi), mounted
	// into the viewer's session bar.
	Controls template.HTML
}

// RenderScreen renders the viewer for one desktop's screen.
func (rd *Renderer) RenderScreen(w io.Writer, data ScreenPageData) error {
	return rd.screenTmpl.ExecuteTemplate(w, "screen.html", data)
}

// FaviconSVG is the Decurix brand mark, served as the control plane's favicon.
// It is re-exported here so httpapi keeps reaching web assets through webui.
var FaviconSVG = web.FaviconSVG

// NoVNC is the screen viewer's RFB client (web/novnc/rfb.js), re-exported for
// httpapi the same way.
var NoVNC = web.NoVNC
