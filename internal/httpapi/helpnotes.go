package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/YKoustubhRao/flowera/internal/helpnotes"
	"github.com/YKoustubhRao/flowera/internal/webui"
)

// helpTextHandler records a help note submitted from inside a desktop.
func (s *Server) helpTextHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		DesktopID string `json:"desktop_id"`
		VMName    string `json:"vm_name"`
		Filename  string `json:"filename"`
		Content   string `json:"content"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if req.DesktopID == "" {
		http.Error(w, "desktop_id is required", http.StatusBadRequest)
		return
	}

	if len(req.Content) > 16384 {
		http.Error(w, "content exceeds maximum size of 16384 bytes", http.StatusBadRequest)
		return
	}

	note := helpnotes.HelpNote{
		ID:        fmt.Sprintf("%d", time.Now().UnixNano()),
		DesktopID: req.DesktopID,
		VMName:    req.VMName,
		Filename:  req.Filename,
		Content:   req.Content,
		CreatedAt: time.Now(),
	}

	s.HelpNotes.Add(note)

	w.Header().Set("Content-Type", "application/json")

	json.NewEncoder(w).Encode(map[string]string{
		"status": "received",
	})
}

// helpNotesHandler returns all recorded help notes as JSON.
func (s *Server) helpNotesHandler(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, s.HelpNotes.List())
}

// closeHelpNoteHandler deletes a help note by ID.
func (s *Server) closeHelpNoteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	id := r.FormValue("id")

	s.HelpNotes.Close(id)

	jsonResponse(w, map[string]string{
		"status": "closed",
	})
}

// dashboardPostsHandler renders the help-notes dashboard, most recent first.
func (s *Server) dashboardPostsHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/dashboard" {
		http.NotFound(w, r)
		return
	}

	notes := s.HelpNotes.List()

	sort.Slice(notes, func(i, j int) bool {
		return notes[i].CreatedAt.After(notes[j].CreatedAt)
	})

	data := webui.HelpNotesPageData{
		Notes: notes,
	}

	if err := s.Web.RenderHelpNotesDashboard(w, data); err != nil {
		http.Error(
			w,
			err.Error(),
			http.StatusInternalServerError,
		)
		return
	}
}
