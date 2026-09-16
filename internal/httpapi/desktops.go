package httpapi

import (
	"errors"
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

	linkedMemory := map[string][]string{}
	for _, d := range desktops {
		if linked := s.Memory.LinkedWorkers(d.ID); len(linked) > 0 {
			linkedMemory[d.ID] = linked
		}
	}

	data := webui.PageData{
		Desktops:        desktops,
		WorkerMemory:    memories,
		MemoryByDesktop: memoryByDesktop,
		LinkedMemory:    linkedMemory,
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

// createDesktopHandler validates a desktop-creation request (name, profile, cpus, memory, and optionally a specialization/parent_id spawning it as another desktop's child) and creates the desktop.
func (s *Server) createDesktopHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	profile := strings.TrimSpace(r.FormValue("profile"))
	harness := strings.TrimSpace(r.FormValue("harness"))
	// One line: a textarea's newlines would otherwise land in a Docker env
	// var and, later, in identity.py's plain fact list.
	specialization := strings.Join(strings.Fields(r.FormValue("specialization")), " ")
	parentID := strings.TrimSpace(r.FormValue("parent_id"))

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

	// A specialized child worker must be spawned from a desktop that
	// actually exists - otherwise ParentID would name a parent nobody can
	// ever look up.
	if parentID != "" {
		if _, err := s.Desktops.Get(r.Context(), parentID); err != nil {
			if errors.Is(err, desktop.ErrNotFound) {
				http.Error(w, "parent desktop not found", http.StatusBadRequest)
				return
			}

			jsonError(w, fmt.Errorf("parent desktop %s: %w", parentID, err))
			return
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
		specialization,
		parentID,
	)
	if err != nil {
		jsonError(w, err)
		return
	}

	// A child worker starts out sharing L1 memory with the parent it was
	// specialized from, so a focused worker still knows what the original
	// one had already learned. Best-effort: the desktop itself is already
	// created and usable without this.
	if parentID != "" {
		if err := s.Memory.Link(parentID, created.ID); err != nil {
			log.Printf("createDesktop: link child %s to parent %s: %v", created.ID, parentID, err)
		} else {
			s.backfillSharedMemory(r.Context(), parentID, created.ID)
		}
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

	// A schedule targeting a deleted desktop would otherwise keep matching
	// forever, failing silently every time it fires.
	s.Schedules.DeleteForDesktop(desktop.SanitizeID(id))

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
