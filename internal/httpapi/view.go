package httpapi

import (
	"log"
	"net/http"
	"strings"

	"github.com/YKoustubhRao/decurix/internal/ollama"
	"github.com/YKoustubhRao/decurix/internal/webui"
)

// desktopViewHandler renders /view/<id>, a desktop's chat page (desktop.html): the chat and nothing else, the same
// for every harness. A paused or stopped desktop's page offers Wake or Start there; its screen is its own page
// (/desktop/<id>/, screen.go).
func (s *Server) desktopViewHandler(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(
		strings.TrimPrefix(r.URL.Path, "/view/"),
		"/",
	)

	if id == "" {
		http.NotFound(w, r)
		return
	}

	// Someone opening a chat is about to ask for something: have the model
	// resident by the time they have typed it.
	ollama.WarmAsync()

	d, ok := s.pageDesktop(w, r, id, "desktop view")
	if !ok {
		return
	}

	data := webui.DesktopViewPageData{
		Desktop: d,
	}

	if err := s.Web.RenderDesktopView(w, data); err != nil {
		log.Printf("desktop view: render: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}
