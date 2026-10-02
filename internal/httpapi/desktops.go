package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/memory"
	"github.com/YKoustubhRao/decurix/internal/ollama"
	"github.com/YKoustubhRao/decurix/internal/tasks"
	"github.com/YKoustubhRao/decurix/internal/webui"
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

	linkedAll := s.Memory.LinkedWorkersMap()
	linkedMemory := map[string][]string{}
	desktopNames := map[string]string{}
	for _, d := range desktops {
		if linked := linkedAll[d.ID]; len(linked) > 0 {
			linkedMemory[d.ID] = linked
		}
		desktopNames[d.ID] = d.Name
	}

	data := webui.PageData{
		Desktops:        desktops,
		WorkerMemory:    memories,
		MemoryByDesktop: memoryByDesktop,
		LinkedMemory:    linkedMemory,
		DesktopNames:    desktopNames,
		TunnelURL:       s.Tunnel.Get(),
		Native:          s.Desktops.Native(),
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
	brain, brainErr := s.desktopBrain(r.FormValue("harness"), r.FormValue("brain"))
	harness := desktop.HarnessTheBot
	// One line: a textarea's newlines would otherwise land in a Docker env
	// var and, later, in identity.py's plain fact list.
	specialization := strings.Join(strings.Fields(r.FormValue("specialization")), " ")
	parentID := strings.TrimSpace(r.FormValue("parent_id"))

	cpus := int64(1)
	if envCpus := os.Getenv("DECURIX_DEFAULT_DESKTOP_CPUS"); envCpus != "" {
		if parsed, err := strconv.ParseInt(envCpus, 10, 64); err == nil && parsed > 0 {
			cpus = parsed
		}
	}

	memoryMB := int64(2048)
	if envMem := os.Getenv("DECURIX_DEFAULT_DESKTOP_MEMORY_MB"); envMem != "" {
		if parsed, err := strconv.ParseInt(envMem, 10, 64); err == nil && parsed > 0 {
			memoryMB = parsed
		}
	}

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

	if brainErr != nil {
		jsonStatusError(w, http.StatusBadRequest, brainErr.Error())
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

	// Clamp cpus so a desktop container never completely starves the host OS
	maxHostCpus := int64(runtime.NumCPU())
	if maxHostCpus > 2 {
		maxHostCpus -= 1
	}
	if cpus > maxHostCpus && maxHostCpus > 0 {
		cpus = maxHostCpus
	}

	if memoryMB <= 0 {
		http.Error(w, "memory_mb must be positive", http.StatusBadRequest)
		return
	}

	if memoryMB > 65536 {
		http.Error(w, "memory_mb must not exceed 65536", http.StatusBadRequest)
		return
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

	var selectedApps []string
	copyHostApps := r.FormValue("copy_host_apps") == "1" ||
		strings.EqualFold(r.FormValue("copy_host_apps"), "true")
	if copyHostApps {
		appsVal := strings.TrimSpace(r.FormValue("host_apps"))
		if appsVal != "" {
			for _, part := range strings.Split(appsVal, ",") {
				if trimmed := strings.TrimSpace(part); trimmed != "" {
					selectedApps = append(selectedApps, trimmed)
				}
			}
		}
	}

	if r.Context().Err() != nil {
		log.Printf("createDesktop: client canceled before creation for %s", name)
		return
	}

	// The chat model is baked into a desktop when it is created. With no
	// desktop yet, choose it against the machine as it is now; with any,
	// keep the model they already run (see ollama.Reselect).
	existing, listErr := s.Desktops.List(r.Context())
	ollama.Reselect(r.Context(), listErr != nil || len(existing) > 0)

	prov, err := s.Desktops.BeginProvisioning(
		name,
		profile,
		harness,
		brain,
		cpus,
		memoryMB,
		"",
		specialization,
		parentID,
		selectedApps,
		func(created *desktop.Desktop) error {
			if parentID != "" {
				if err := s.Memory.Link(parentID, created.ID); err != nil {
					log.Printf("createDesktop: link child %s to parent %s: %v", created.ID, parentID, err)
				} else {
					s.backfillSharedMemory(context.Background(), parentID, created.ID)
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
			return nil
		},
	)
	if err != nil {
		if errors.Is(err, context.Canceled) || r.Context().Err() != nil {
			log.Printf("createDesktop: request canceled by client, aborting creation for %s", name)
			return
		}
		jsonError(w, err)
		return
	}

	w.WriteHeader(http.StatusAccepted)
	jsonResponse(w, prov)
}

// desktopStatusHandler returns the current provisioning and readiness status for a desktop.
func (s *Server) desktopStatusHandler(w http.ResponseWriter, r *http.Request) {
	desktopID := strings.TrimSpace(r.URL.Query().Get("id"))
	if desktopID == "" {
		http.Error(w, "missing id parameter", http.StatusBadRequest)
		return
	}

	// 1. Check in-flight provisioning state
	if prov, ok := s.Desktops.GetProvisioning(desktopID); ok {
		jsonResponse(w, map[string]any{
			"id":       prov.ID,
			"name":     prov.Name,
			"status":   prov.Status,
			"stage":    prov.Stage,
			"message":  prov.Message,
			"progress": prov.Progress,
			"ready":    prov.Ready,
			"url":      prov.URL,
			"error":    prov.Error,
		})
		return
	}

	// 2. Otherwise inspect the desktop
	d, err := s.Desktops.Get(r.Context(), desktopID)
	if err != nil {
		if errors.Is(err, desktop.ErrNotFound) {
			http.Error(w, "desktop not found", http.StatusNotFound)
			return
		}
		jsonError(w, err)
		return
	}

	isReady := d.Status == "running"
	jsonResponse(w, map[string]any{
		"id":       d.ID,
		"name":     d.Name,
		"status":   d.Status,
		"stage":    "ready",
		"message":  "Desktop is running",
		"progress": 100,
		"ready":    isReady,
		"url":      d.URL,
	})
}

// startDesktopHandler starts an existing desktop by ID.
func (s *Server) startDesktopHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	// The model takes minutes to load cold and the desktop seconds to
	// start: begin loading it now so the two overlap.
	ollama.WarmAsync()

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

// pauseDesktopHandler freezes a running desktop by ID using the cgroup freezer (0% CPU).
func (s *Server) pauseDesktopHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	id := r.FormValue("id")

	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}

	if err := s.Desktops.Pause(r.Context(), id); err != nil {
		jsonError(w, err)
		return
	}

	jsonResponse(w, map[string]string{
		"status": "paused",
	})
}

// unpauseDesktopHandler thaws a paused desktop by ID (< 1ms).
func (s *Server) unpauseDesktopHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	// The model takes minutes to load cold and the desktop seconds to
	// start: begin loading it now so the two overlap.
	ollama.WarmAsync()

	id := r.FormValue("id")

	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}

	if err := s.Desktops.Unpause(r.Context(), id); err != nil {
		jsonError(w, err)
		return
	}

	jsonResponse(w, map[string]string{
		"status": "running",
	})
}

// restartDesktopHandler restarts a desktop by ID.
func (s *Server) restartDesktopHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	// The model takes minutes to load cold and the desktop seconds to
	// start: begin loading it now so the two overlap.
	ollama.WarmAsync()

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

	// Its open notes can never be answered now.
	s.QueryNotes.ResolveDesktop(desktop.SanitizeID(id))

	// The worker's records outlive its container.
	if err := s.Tasks.MarkWorkerDeleted(desktop.SanitizeID(id)); err != nil {
		log.Printf("deleteDesktop: %v", err)
	}

	// Its chat does not: a desktop made later under the same name is another
	// worker, and opened with this one's whole conversation in it (found by
	// deleting "eve" and creating "eve"). The task records above keep what
	// was done.
	s.Chat.Clear(desktop.SanitizeID(id))

	// Nor does what it learned, for the same reason: the new worker's card
	// listed the old one's memory, a skill among it that it did not have.
	// (Its skills and memory files went with its home.) A backup taken
	// first is how know-how is carried to another worker.
	forgotten, err := s.Memory.DeleteForDesktop(desktop.SanitizeID(id))
	if err != nil {
		log.Printf("deleteDesktop: %v", err)
	}
	if s.Suggest != nil {
		s.Suggest.Forget(forgotten)
	}

	// A schedule targeting a deleted desktop would otherwise keep matching
	// forever, failing silently every time it fires.
	s.Schedules.DeleteForDesktop(desktop.SanitizeID(id))

	// Likewise an email rule: every matching email would queue a job that
	// can never be delivered.
	if s.Mail != nil {
		s.Mail.DeleteForDesktop(desktop.SanitizeID(id))
	}

	// Its Windows apps were kept running on the host for it; nobody can open them now.
	s.forgetHostApps(desktop.SanitizeID(id))

	jsonResponse(w, map[string]string{
		"status": "deleted",
	})
}

// closeOpenTasks ends the task records and the chat turn a desktop's agent can no longer finish:
// stopping, restarting or deleting a desktop throws away what its agent was
// in the middle of, including a pause waiting on a human.
func (s *Server) closeOpenTasks(id, why string) {
	if err := s.Tasks.CloseOpen(desktop.SanitizeID(id), why); err != nil {
		log.Printf("closing tasks for %s: %v", id, err)
	}

	// And the chat's turn for it, which nothing else would close now.
	s.Chat.EndTurn(desktop.SanitizeID(id), why)
}
