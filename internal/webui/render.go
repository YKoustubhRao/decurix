// Package webui renders the HTML dashboard/requests pages.
package webui

import (
	"fmt"
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
	// Gated: the page is served on the gated listener; owner-only controls (Simulate) are hidden.
	Gated                  bool
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
	// TasksByDesktop maps a desktop ID to its tasks, newest first - what the
	// card's task dropdown shows before its first poll of /api/desktops/tasks.
	TasksByDesktop map[string][]tasks.Task
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
	// Purge shows the "Purge a leaked secret" control (POST /api/privacy/purge): on the primary listener only, where
	// that route is served.
	Purge bool
}

// TaskPageData is the template data for one task record.
type TaskPageData struct {
	Task         tasks.Detail
	ReadinessURL string
}

// TaskTracePageData is the template data for one task's full trace: the
// request, every attempt and every step the store holds, in order.
type TaskTracePageData struct {
	Task tasks.Detail
	// RunRecords offers each attempt's run record for download (/api/runs/<execution>/bundle): on the primary
	// listener only, where that route is served.
	RunRecords bool
}

// ShortTitleRunes is how much of a request the dashboard's task dropdown shows.
const ShortTitleRunes = 60

// foldLines and foldRunes are the size past which a step's text is folded
// on the trace page, with its first line as the summary.
const (
	foldLines = 8
	foldRunes = 700
)

// StepKind names what a step is, from the generic prefixes every harness
// reports with (progress.emit): a command, its output, an on-screen action,
// or a note (plans, checks, gate decisions, anything else said in words).
func StepKind(step tasks.Step) string {
	switch {
	case step.Action != nil:
		return "screen"
	case strings.HasPrefix(step.Text, "$ "), strings.HasPrefix(step.Text, "Running: "):
		return "command"
	case strings.HasPrefix(step.Text, "Output: "):
		return "output"
	default:
		return "note"
	}
}

// stepKindLabel is the word the trace shows for a StepKind.
var stepKindLabel = map[string]string{
	"screen":  "Screen",
	"command": "Command",
	"output":  "Output",
	"note":    "Note",
}

// firstLine is the first line of a text, clipped to one line of at most n runes.
func firstLine(text string, n int) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	return tasks.ShortTitle(line, n)
}

// isLong says whether a step's text is folded on the trace page.
func isLong(text string) bool {
	return strings.Count(text, "\n")+1 > foldLines || len([]rune(text)) > foldRunes
}

// TraceText renders a task's whole trace as plain text - the request, each
// attempt with its model, timing, tokens and cost, every step with its
// timestamp, every question and answer, and the final report - for a copy
// button or a download, so a run can be shared as one file.
func TraceText(detail tasks.Detail) string {
	var b strings.Builder
	stamp := func(t time.Time) string {
		if t.IsZero() {
			return "—"
		}
		return t.UTC().Format("2006-01-02 15:04:05Z")
	}
	offset := func(from, to time.Time) string {
		if from.IsZero() || to.IsZero() {
			return ""
		}
		return "+" + tasks.FormatDuration(to.Sub(from))
	}

	fmt.Fprintf(&b, "Task %s\n", detail.ID)
	fmt.Fprintf(&b, "Worker: %s (%s)", detail.WorkerName, detail.WorkerID)
	if !detail.Worker.DeletedAt.IsZero() {
		b.WriteString(" - deleted")
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "Engine: %s\n", tasks.EngineLabel(detail.Engine))
	fmt.Fprintf(&b, "Type: %s\n", tasks.TypeLabel(detail.Type))
	fmt.Fprintf(&b, "Status: %s\n", tasks.DisplayOutcome(detail.Status, detail.Checked).Label)
	fmt.Fprintf(&b, "Created: %s\n", stamp(detail.CreatedAt))
	if detail.Verdict != "" {
		fmt.Fprintf(&b, "Verdict: %s (%s)\n", detail.Verdict, stamp(detail.VerdictAt))
		if detail.VerdictNote != "" {
			fmt.Fprintf(&b, "Verdict note: %s\n", detail.VerdictNote)
		}
	}
	fmt.Fprintf(&b, "\nRequest:\n%s\n", detail.Request)

	for _, e := range detail.Executions {
		shown := tasks.DisplayOutcome(e.Status, e.Checked)
		fmt.Fprintf(&b, "\n==== Attempt %d: %s", e.Attempt, shown.Label)
		fmt.Fprintf(&b, " ====\nEngine: %s", tasks.EngineLabel(e.Engine))
		if e.Model != "" {
			fmt.Fprintf(&b, " · %s", e.Model)
		}
		fmt.Fprintf(&b, "\nStarted: %s\nEnded: %s\n", stamp(e.StartedAt), stamp(e.EndedAt))
		if !e.EndedAt.IsZero() {
			fmt.Fprintf(&b, "Duration: %s\n", tasks.FormatDuration(e.EndedAt.Sub(e.StartedAt)))
		}
		if e.InputTokens != nil || e.OutputTokens != nil {
			fmt.Fprintf(&b, "Tokens: %s in / %s out\n", tasks.FormatTokens(e.InputTokens), tasks.FormatTokens(e.OutputTokens))
		}
		if e.CostUSD != nil {
			fmt.Fprintf(&b, "Cost: %s\n", tasks.FormatCost(e.CostUSD))
		}
		if e.Checked != nil || e.Status == tasks.StatusDone {
			fmt.Fprintf(&b, "%s\n", FinalCheck(e.Checked))
		}

		b.WriteString("\n-- Steps --\n")
		n := 0
		for _, ev := range e.Timeline() {
			if ev.Step != nil {
				n++
				s := ev.Step
				fmt.Fprintf(&b, "[%d] %s %s  %s\n", n, stamp(s.CreatedAt), offset(e.StartedAt, s.CreatedAt), stepKindLabel[StepKind(*s)])
				b.WriteString(indent(s.Text))
				if s.Action != nil {
					a := s.Action
					fmt.Fprintf(&b, "    action: %s", a.Kind)
					if a.Target != "" {
						fmt.Fprintf(&b, " %s", a.Target)
					}
					if a.Outcome != "" {
						fmt.Fprintf(&b, " -> %s", a.Outcome)
					}
					if a.Change != "" {
						fmt.Fprintf(&b, "; change: %s", a.Change)
					}
					if a.Reason != "" {
						fmt.Fprintf(&b, "; reason: %s", a.Reason)
					}
					b.WriteString("\n")
				}
			}
			if ev.Intervention != nil {
				i := ev.Intervention
				kind := "Query"
				if i.Kind == tasks.InterventionApproval {
					kind = "Approval"
				}
				fmt.Fprintf(&b, "[?] %s %s  %s asked (%s)\n", stamp(i.RequestedAt), offset(e.StartedAt, i.RequestedAt), kind, tasks.ReasonLabel(i.Reason))
				b.WriteString(indent(i.Request))
				if i.Reply != "" {
					fmt.Fprintf(&b, "    Reply at %s:\n", stamp(i.RepliedAt))
					b.WriteString(indent(i.Reply))
				} else {
					b.WriteString("    (no reply yet)\n")
				}
			}
		}
		if n == 0 {
			b.WriteString("(no steps were reported)\n")
		}

		if e.FinalReply != "" {
			fmt.Fprintf(&b, "\n-- Final report (%s) --\n%s\n", shown.Label, e.FinalReply)
		}
	}

	return b.String()
}

// FinalCheck says what a desktop's final check did with a result, from the records' flag: judged it, did not run,
// or said nothing (an older image).
func FinalCheck(checked *bool) string {
	switch {
	case checked == nil:
		return "Final check: not reported"
	case *checked:
		return "Final check: judged"
	default:
		return "Final check: not run"
	}
}

// indent writes text with every line indented four spaces, ending in a newline.
func indent(text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i, line := range lines {
		lines[i] = "    " + line
	}
	return strings.Join(lines, "\n") + "\n"
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
	// Gated is a page served on the phone/tunnel listener, which offers no host-only action (cancelling a run).
	Gated bool
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
	// outcome is what a person is shown for a status and the final check's word together: the only label a page
	// puts on how a task ended (a done nobody's check judged reads "Done, not checked").
	"outcome":     tasks.DisplayOutcome,
	"contains":    strings.Contains,
	"reasonLabel": tasks.ReasonLabel,
	"duration":    tasks.FormatDuration,
	"cost":        tasks.FormatCost,
	"tokens":      tasks.FormatTokens,
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
	// finalCheck says what the final check did, from the *bool the records keep (deref is for *int64 figures:
	// using it on Checked crashed the trace page on every reported check, W-02).
	"finalCheck": FinalCheck,
	"live": func(status string) bool {
		return status == tasks.StatusQueued || status == tasks.StatusRunning || status == tasks.StatusWaiting
	},
	"shortTitle": func(request string) string { return tasks.ShortTitle(request, ShortTitleRunes) },
	// The trace page's helpers: what a step is, whether to fold it, and its
	// first line as the fold's summary.
	"stepKind":      StepKind,
	"stepKindLabel": func(kind string) string { return stepKindLabel[kind] },
	"isLong":        isLong,
	"firstLine":     func(text string) string { return firstLine(text, 160) },
	"lines":         func(text string) int { return strings.Count(strings.TrimRight(text, "\n"), "\n") + 1 },
	"stamp": func(t time.Time) string {
		if t.IsZero() {
			return "—"
		}
		return t.UTC().Format("15:04:05")
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
	traceTmpl      *template.Template
	recordingsTmpl *template.Template
	skillsTmpl     *template.Template
	readinessTmpl  *template.Template
	dagsTmpl       *template.Template
	dagTmpl        *template.Template
	hostGUITmpl    *template.Template
	phoneAppTmpl   *template.Template
	phoneLinkTmpl  *template.Template
	screenTmpl     *template.Template
	triggersTmpl   *template.Template
	conciergeTmpl  *template.Template
}

// NewRenderer parses all templates once at startup.
func NewRenderer() *Renderer {
	return &Renderer{
		readinessTmpl:  parse(nil, "readiness.html"),
		indexTmpl:      parse(taskFuncs, "index.html"),
		requestsTmpl:   parse(nil, "requests.html", triggerParts),
		dashboardTmpl:  parse(nil, "dashboard.html"),
		desktopTmpl:    parse(nil, "desktop.html"),
		tasksTmpl:      parse(taskFuncs, "tasks.html"),
		taskTmpl:       parse(taskFuncs, "task.html"),
		traceTmpl:      parse(taskFuncs, "trace.html"),
		recordingsTmpl: parse(nil, "recordings.html"),
		skillsTmpl:     parse(nil, "skills.html"),
		dagsTmpl:       parse(taskFuncs, "dags.html"),
		dagTmpl:        parse(taskFuncs, "dag.html"),
		hostGUITmpl:    parse(nil, "hostgui.html"),
		phoneAppTmpl: template.Must(
			template.ParseFS(web.TemplatesFS, "templates/phoneapp.html"),
		),
		phoneLinkTmpl: template.Must(
			template.ParseFS(web.TemplatesFS, "templates/phonelink.html"),
		),
		screenTmpl:   parse(nil, "screen.html"),
		triggersTmpl: parse(nil, "triggers.html", triggerParts),
		conciergeTmpl: template.Must(
			template.ParseFS(web.TemplatesFS, templatePaths("concierge.html")...),
		),
	}
}

// parse parses a page with the shared theme and any partials, with funcs (may
// be nil) available to them.
func parse(funcs template.FuncMap, page string, partials ...string) *template.Template {
	return template.Must(template.New(page).Funcs(funcs).ParseFS(web.TemplatesFS, templatePaths(page, partials...)...))
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

// RenderTaskTrace renders one task's full trace page.
func (rd *Renderer) RenderTaskTrace(w io.Writer, data TaskTracePageData) error {
	return rd.traceTmpl.ExecuteTemplate(w, "trace.html", data)
}

// RenderRecordings renders one desktop's recordings page.
func (rd *Renderer) RenderRecordings(w io.Writer, data RecordingsPageData) error {
	return rd.recordingsTmpl.ExecuteTemplate(w, "recordings.html", data)
}

// RenderSkills renders one desktop's skills page.
func (rd *Renderer) RenderSkills(w io.Writer, data SkillsPageData) error {
	return rd.skillsTmpl.ExecuteTemplate(w, "skills.html", data)
}

// ReadinessPageData identifies a worker and the local owner-key file name.
type ReadinessPageData struct {
	Desktop   desktop.Desktop
	OwnerFile string
}

// RenderReadiness renders the owner's qualification and evidence console.
func (rd *Renderer) RenderReadiness(w io.Writer, data ReadinessPageData) error {
	return rd.readinessTmpl.ExecuteTemplate(w, "readiness.html", data)
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

// RenderConcierge renders the chat-first page, /chat. It takes no data:
// the page reads the conversation from /api/concierge.
func (rd *Renderer) RenderConcierge(w io.Writer) error {
	return rd.conciergeTmpl.ExecuteTemplate(w, "concierge.html", nil)
}

// FaviconSVG is the Decurix brand mark, served as the control plane's favicon.
// It is re-exported here so httpapi keeps reaching web assets through webui.
var FaviconSVG = web.FaviconSVG

// NoVNC is the screen viewer's RFB client (web/novnc/rfb.js), re-exported for
// httpapi the same way.
var NoVNC = web.NoVNC
