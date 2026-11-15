package httpapi

import (
	"log"
	"net/http"

	"github.com/YKoustubhRao/decurix/internal/webui"
)

// workforcePageHandler renders the workforce page (GET /workforce). The queue,
// handoff and access APIs are registered on the primary listener only, so a
// request through the gated listener gets the page without those sections.
func (s *Server) workforcePageHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/workforce" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	gated := isGated(r)
	data := webui.WorkforcePageData{
		Gated:        gated,
		HasSkillHub:  s.SkillHub != nil,
		HasKnowledge: s.Knowledge != nil,
		HasQueue:     s.Queue != nil && !gated,
		HasHandoffs:  s.Handoffs != nil && !gated,
		HasAccess:    s.access() != nil && !gated,
		HasProposals: s.Knowledge != nil && !gated,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.Web.RenderWorkforce(w, data); err != nil {
		log.Printf("workforce page: %v", err)
		http.Error(w, "could not render the page", http.StatusInternalServerError)
	}
}
