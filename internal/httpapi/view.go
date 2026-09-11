package httpapi

import (
	"net/http"
	"strings"

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

	desktops, err := s.Desktops.List(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	for _, d := range desktops {
		if d.ID != id {
			continue
		}

		data := webui.DesktopViewPageData{
			Desktop: d,
			Host:    getHost(r),
		}

		if err := s.Web.RenderDesktopView(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}

		return
	}

	http.NotFound(w, r)
}
