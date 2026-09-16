package httpapi

import (
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/YKoustubhRao/flowera/internal/desktop"
	"github.com/YKoustubhRao/flowera/internal/webui"
)

// desktopViewHandler renders a single desktop's detail page: a Desktop/Agent
// tab switcher (Agent only for harness=thebot, the only harness with
// anything listening on agentPort) plus a side-by-side split view.
func (s *Server) desktopViewHandler(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(
		strings.TrimPrefix(r.URL.Path, "/view/"),
		"/",
	)

	if id == "" {
		http.NotFound(w, r)
		return
	}

	d, err := s.Desktops.Get(r.Context(), id)
	if errors.Is(err, desktop.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		log.Printf("desktop view: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	data := webui.DesktopViewPageData{
		Desktop: d,
	}

	if err := s.Web.RenderDesktopView(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
