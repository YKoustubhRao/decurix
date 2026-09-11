// Package webui renders the HTML dashboard/messages pages.
package webui

import (
	"html/template"
	"io"
	"path"

	"github.com/YKoustubhRao/flowera/internal/desktop"
	"github.com/YKoustubhRao/flowera/internal/helpnotes"
	"github.com/YKoustubhRao/flowera/internal/memory"
	"github.com/YKoustubhRao/flowera/web"
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
func templatePaths(page string) []string {
	return []string{
		path.Join(templatesDir, page),
		path.Join(templatesDir, themeFile),
	}
}

// PageData is the template data passed to the dashboard page: the current desktops, the worker memory, the request host, and the current cloud tunnel URL (if any).
type PageData struct {
	Desktops        []desktop.Desktop
	WorkerMemory    []memory.WorkerMemoryEntry
	MemoryByDesktop map[string][]memory.WorkerMemoryEntry
	Host            string
	TunnelURL       string
}

// HelpNotesPageData is the template data for the help-notes dashboard.
type HelpNotesPageData struct {
	Notes []helpnotes.HelpNote
}

// DesktopViewPageData is the template data for a single desktop's detail
// page: its Desktop/Agent tab switcher and split view.
type DesktopViewPageData struct {
	Desktop desktop.Desktop
	Host    string
}

// Renderer loads and executes the web UI's HTML templates.
type Renderer struct {
	indexTmpl *template.Template
}

// NewRenderer parses the templates that are safe to parse once at startup.
func NewRenderer() *Renderer {
	return &Renderer{
		indexTmpl: template.Must(
			template.ParseFS(web.TemplatesFS, templatePaths("index.html")...),
		),
	}
}

// RenderIndex renders the root dashboard page.
func (rd *Renderer) RenderIndex(w io.Writer, data PageData) error {
	return rd.indexTmpl.ExecuteTemplate(w, "index.html", data)
}

// RenderMessages renders the messages page. The template is re-parsed on
// every call, matching the original handler's behavior.
func (rd *Renderer) RenderMessages(w io.Writer, data PageData) error {
	tmpl := template.Must(
		template.ParseFS(web.TemplatesFS, templatePaths("messages.html")...),
	)

	return tmpl.ExecuteTemplate(w, "messages.html", data)
}

// RenderHelpNotesDashboard renders the help-notes dashboard. The template is
// re-parsed on every call, matching the original handler's behavior.
func (rd *Renderer) RenderHelpNotesDashboard(w io.Writer, data HelpNotesPageData) error {
	tmpl := template.Must(
		template.ParseFS(web.TemplatesFS, templatePaths("dashboard.html")...),
	)

	return tmpl.ExecuteTemplate(w, "dashboard.html", data)
}

// RenderDesktopView renders a single desktop's detail page (Desktop/Agent
// tabs, split view). The template is re-parsed on every call, matching the
// other secondary pages' behavior.
func (rd *Renderer) RenderDesktopView(w io.Writer, data DesktopViewPageData) error {
	tmpl := template.Must(
		template.ParseFS(web.TemplatesFS, templatePaths("desktop.html")...),
	)

	return tmpl.ExecuteTemplate(w, "desktop.html", data)
}

// FaviconSVG is the Flowera brand mark, served as the control plane's favicon.
// It is re-exported here so httpapi keeps reaching web assets through webui.
var FaviconSVG = web.FaviconSVG
