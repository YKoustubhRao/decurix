package webui

import "io"

// WorkforcePageData is the template data for the workforce page: which of its
// sections this listener serves. Everything the sections show is read by the
// page's own script from the JSON APIs and polled, so the page holds no state.
type WorkforcePageData struct {
	// Gated says the page was asked for through the gated listener; the
	// sections that only the primary listener serves are left out.
	Gated bool
	// HasSkillHub, HasKnowledge, HasQueue, HasHandoffs and HasAccess say which
	// APIs are wired and reachable from this listener; a section without its
	// API is not rendered.
	HasSkillHub  bool
	HasKnowledge bool
	HasQueue     bool
	HasHandoffs  bool
	HasAccess    bool
	// HasProposals says the list of what workers proposed, with Approve and Reject, is served here: the primary
	// listener only, like every knowledge write (O-7).
	HasProposals bool
}

// workforceTmpl is parsed once at start-up, as NewRenderer parses the rest.
var workforceTmpl = parse(nil, "workforce.html")

// RenderWorkforce renders the workforce page: workers and roles, the skill
// registry, the work queue, handoffs, knowledge and access.
func (rd *Renderer) RenderWorkforce(w io.Writer, data WorkforcePageData) error {
	return workforceTmpl.ExecuteTemplate(w, "workforce.html", data)
}
