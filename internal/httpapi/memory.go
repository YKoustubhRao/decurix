package httpapi

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/YKoustubhRao/flowera/internal/memory"
)

// desktopMemoryHandler stores a new worker memory entry or returns all remembered workflows.
func (s *Server) desktopMemoryHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		jsonResponse(w, s.Memory.List(""))
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	desktopID := strings.TrimSpace(r.FormValue("desktop_id"))
	title := strings.TrimSpace(r.FormValue("title"))
	summary := strings.TrimSpace(r.FormValue("summary"))
	category := strings.TrimSpace(r.FormValue("category"))

	entry, err := s.Memory.Record(desktopID, title, summary, category)
	if err != nil {
		jsonError(w, err)
		return
	}

	filename, content := memory.FormatForDesktopFile(entry)

	if err := s.Desktops.PersistMemoryFile(
		r.Context(),
		entry.DesktopID,
		filename,
		content,
	); err != nil {
		jsonError(w, fmt.Errorf("persist memory: %w", err))
		return
	}

	jsonResponse(w, entry)
}
