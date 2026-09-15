package httpapi

import (
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/YKoustubhRao/flowera/internal/desktop"
	"github.com/YKoustubhRao/flowera/internal/memory"
	"github.com/YKoustubhRao/flowera/internal/tasks"
	"github.com/YKoustubhRao/flowera/internal/webui"
)

var validDesktopNameRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// dashboardHandler renders the root dashboard page listing all desktops.
func (s *Server) dashboardHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	desktops, err := s.Desktops.List(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	memories := s.Memory.List("")
	memoryByDesktop := map[string][]memory.WorkerMemoryEntry{}
	for _, entry := range memories {
		memoryByDesktop[entry.DesktopID] = append(memoryByDesktop[entry.DesktopID], entry)
	}

	data := webui.PageData{
		Desktops:        desktops,
		WorkerMemory:    memories,
		MemoryByDesktop: memoryByDesktop,
		TunnelURL:       s.Tunnel.Get(),
	}

	if err := s.Web.RenderIndex(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// desktopsAPIHandler returns the current list of desktops as JSON.
func (s *Server) desktopsAPIHandler(w http.ResponseWriter, r *http.Request) {
	desktops, err := s.Desktops.List(r.Context())
	if err != nil {
		jsonError(w, err)
		return
	}

	jsonResponse(w, desktops)
}

// createDesktopHandler validates a desktop-creation request (name, profile, cpus, memory) and creates the desktop.
func (s *Server) createDesktopHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	profile := strings.TrimSpace(r.FormValue("profile"))
	harness := strings.TrimSpace(r.FormValue("harness"))

	cpus := int64(2)
	memoryMB := int64(4096)

	if profile == "" {
		profile = "basic"
	}

	switch profile {
	case "basic", "developer", "ai":
		// valid
	default:
		http.Error(
			w,
			"invalid profile: must be basic, developer, or ai",
			http.StatusBadRequest,
		)
		return
	}

	if harness == "" {
		harness = desktop.HarnessOllama
	}

	switch harness {
	case desktop.HarnessOllama, desktop.HarnessClaude, desktop.HarnessTheBot:
		// valid
	default:
		http.Error(
			w,
			"invalid harness: must be ollama, claude, or thebot",
			http.StatusBadRequest,
		)
		return
	}

	if profile == "basic" && harness == desktop.HarnessClaude {
		http.Error(
			w,
			"claude harness requires the developer or ai profile",
			http.StatusBadRequest,
		)
		return
	}

	if v := r.FormValue("cpus"); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil && parsed > 0 {
			cpus = parsed
		}
	}

	if v := r.FormValue("memory_mb"); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil && parsed > 0 {
			memoryMB = parsed
		}
	}

	if name == "" {
		name = fmt.Sprintf("desktop-%d", time.Now().Unix())
	}

	if !validDesktopNameRe.MatchString(name) {
		http.Error(w, "invalid name: only alphanumeric, hyphens, underscores, and dots allowed", http.StatusBadRequest)
		return
	}

	if cpus <= 0 {
		http.Error(w, "cpus must be positive", http.StatusBadRequest)
		return
	}

	if cpus > 64 {
		http.Error(w, "cpus must not exceed 64", http.StatusBadRequest)
		return
	}

	if memoryMB <= 0 {
		http.Error(w, "memory_mb must be positive", http.StatusBadRequest)
		return
	}

	if memoryMB > 65536 {
		http.Error(w, "memory_mb must not exceed 65536", http.StatusBadRequest)
		return
	}

	var claudeCredentialsJSON string
	if harness == desktop.HarnessClaude {
		if session, ok := s.ClaudeAuth.Get(); ok {
			claudeCredentialsJSON = session.CredentialsJSON
		}
	}

	created, err := s.Desktops.Create(
		r.Context(),
		name,
		profile,
		harness,
		cpus,
		memoryMB,
		claudeCredentialsJSON,
	)
	if err != nil {
		jsonError(w, err)
		return
	}

	if err := s.Tasks.UpsertWorker(tasks.Worker{
		ID:      created.ID,
		Name:    created.Name,
		Engine:  created.Harness,
		Profile: created.Profile,
		Model:   created.Model,
	}); err != nil {
		log.Printf("createDesktop: %v", err)
	}

	jsonResponse(w, created)
}

// startDesktopHandler starts an existing desktop by ID.
func (s *Server) startDesktopHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	id := r.FormValue("id")

	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}

	if err := s.Desktops.Start(r.Context(), id); err != nil {
		jsonError(w, err)
		return
	}

	jsonResponse(w, map[string]string{
		"status": "started",
	})
}

// stopDesktopHandler stops a running desktop by ID.
func (s *Server) stopDesktopHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	id := r.FormValue("id")

	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}

	if err := s.Desktops.Stop(r.Context(), id); err != nil {
		jsonError(w, err)
		return
	}

	s.closeOpenTasks(id, "The desktop was stopped before this task finished.")

	jsonResponse(w, map[string]string{
		"status": "stopped",
	})
}

// restartDesktopHandler restarts a desktop by ID.
func (s *Server) restartDesktopHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	id := r.FormValue("id")

	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}

	if err := s.Desktops.Restart(r.Context(), id); err != nil {
		jsonError(w, err)
		return
	}

	s.closeOpenTasks(id, "The desktop was restarted before this task finished.")

	jsonResponse(w, map[string]string{
		"status": "restarted",
	})
}

// deleteDesktopHandler deletes a desktop and its persistent storage by ID.
func (s *Server) deleteDesktopHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	id := r.FormValue("id")

	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}

	if err := s.Desktops.Delete(r.Context(), id); err != nil {
		jsonError(w, err)
		return
	}

	s.closeOpenTasks(id, "The desktop was deleted before this task finished.")

	// The worker's records outlive its container.
	if err := s.Tasks.MarkWorkerDeleted(desktop.SanitizeID(id)); err != nil {
		log.Printf("deleteDesktop: %v", err)
	}

	jsonResponse(w, map[string]string{
		"status": "deleted",
	})
}

// closeOpenTasks ends the task records a desktop's agent can no longer finish:
// stopping, restarting or deleting a desktop throws away what its agent was
// in the middle of, including a pause waiting on a human.
func (s *Server) closeOpenTasks(id, why string) {
	if err := s.Tasks.CloseOpen(desktop.SanitizeID(id), why); err != nil {
		log.Printf("closing tasks for %s: %v", id, err)
	}
}
