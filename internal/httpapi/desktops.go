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

// positiveInt is raw as a positive number, or fallback when it is not one.
func positiveInt(raw string, fallback int64) int64 {
	if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil && parsed > 0 {
		return parsed
	}
	return fallback
}

// splitList is a comma-separated form value's items, trimmed, without empty ones.
func splitList(raw string) []string {
	var items []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			items = append(items, part)
		}
	}
	return items
}

// pageDesktop looks up the desktop id a page is for, answering the request
// itself when that fails: 404 when there is no such desktop, a 500 (logged
// under what) otherwise.
func (s *Server) pageDesktop(w http.ResponseWriter, r *http.Request, id, what string) (desktop.Desktop, bool) {
	d, err := s.Desktops.Get(r.Context(), id)
	if errors.Is(err, desktop.ErrNotFound) {
		http.NotFound(w, r)
		return d, false
	}
	if err != nil {
		log.Printf("%s: %v", what, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return d, false
	}
	return d, true
}

// desktopFound looks up the desktop id, answering the request itself when that
// fails: missing, in plain text with status, when there is no such desktop,
// jsonError otherwise.
func (s *Server) desktopFound(w http.ResponseWriter, r *http.Request, id string, status int, missing string) (desktop.Desktop, bool) {
	d, err := s.Desktops.Get(r.Context(), id)
	if errors.Is(err, desktop.ErrNotFound) {
		http.Error(w, missing, status)
		return d, false
	}
	if err != nil {
		jsonError(w, err)
		return d, false
	}
	return d, true
}

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
	if !requirePost(w, r) {
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

	// The form's value, else the environment's default, else ours.
	cpus := positiveInt(r.FormValue("cpus"), positiveInt(os.Getenv("DECURIX_DEFAULT_DESKTOP_CPUS"), 1))
	memoryMB := positiveInt(r.FormValue("memory_mb"), positiveInt(os.Getenv("DECURIX_DEFAULT_DESKTOP_MEMORY_MB"), 2048))

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

	if name == "" {
		name = fmt.Sprintf("desktop-%d", time.Now().Unix())
	}

	if !validDesktopNameRe.MatchString(name) {
		http.Error(w, "invalid name: only alphanumeric, hyphens, underscores, and dots allowed", http.StatusBadRequest)
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
		selectedApps = splitList(r.FormValue("host_apps"))
	}

	var hostCopyPaths []desktop.HostCopyPath
	rawHostPaths := strings.TrimSpace(r.FormValue("host_paths"))
	if rawHostPaths == "" {
		rawHostPaths = strings.TrimSpace(r.FormValue("copy_host_paths"))
	}
	if rawHostPaths == "" {
		rawHostPaths = strings.TrimSpace(r.FormValue("copy_paths"))
	}
	if rawHostPaths == "" && len(r.Form["host_paths"]) > 0 {
		rawHostPaths = strings.Join(r.Form["host_paths"], "\n")
	}

	if rawHostPaths != "" {
		parsed, err := desktop.ParseHostCopyPaths(rawHostPaths)
		if err != nil {
			jsonStatusError(w, http.StatusBadRequest, fmt.Sprintf("invalid host copy paths: %v", err))
			return
		}
		for _, p := range parsed {
			if strings.TrimSpace(p.Src) == "" {
				continue
			}
			fi, err := os.Stat(p.Src)
			if err != nil {
				jsonStatusError(w, http.StatusBadRequest, fmt.Sprintf("host path does not exist: %s", p.Src))
				return
			}
			if _, err := desktop.ResolveVMDestPath(p.Src, p.Dest, fi.IsDir()); err != nil {
				jsonStatusError(w, http.StatusBadRequest, fmt.Sprintf("invalid destination path for %s: %v", p.Src, err))
				return
			}
			hostCopyPaths = append(hostCopyPaths, p)
		}
	}

	// One folder of the person's computer the worker may read and write, chosen now and fixed for the desktop's life
	// (desktop.ParseSharedFolder says which are refused, in words the person can act on).
	if strings.TrimSpace(r.FormValue("shared_folder")) != "" && isGated(r) {
		// A folder of the person's own computer is theirs to give, at their own screen: never to whoever holds the
		// access code of the phone/tunnel listener (the same rule as every route that reaches the host).
		jsonStatusError(w, http.StatusForbidden, "a shared folder can only be chosen on this computer, not through the phone or tunnel address")
		return
	}
	sharedFolder, sharedErr := desktop.ParseSharedFolder(r.FormValue("shared_folder"))
	if sharedErr != nil {
		jsonStatusError(w, http.StatusBadRequest, sharedErr.Error())
		return
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
		hostCopyPaths,
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
		sharedFolder,
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
			// Requested host apps it will not have, and why (never a silent skip).
			"missing_apps": prov.MissingApps,
		})
		return
	}

	// 2. Otherwise inspect the desktop
	d, ok := s.desktopFound(w, r, desktopID, http.StatusNotFound, "desktop not found")
	if !ok {
		return
	}

	isReady := d.Status == "running"
	jsonResponse(w, map[string]any{
		"id":           d.ID,
		"name":         d.Name,
		"status":       d.Status,
		"stage":        "ready",
		"message":      "Desktop is running",
		"progress":     100,
		"ready":        isReady,
		"url":          d.URL,
		"missing_apps": d.MissingApps,
	})
}

// desktopAction is the shared start of the start/stop/pause/unpause/restart/
// delete handlers: a POST naming a desktop by id, on which it runs act. It
// answers the request itself when that fails and returns the id and whether
// the action was done. warm begins loading the model first.
func (s *Server) desktopAction(w http.ResponseWriter, r *http.Request, warm bool, act func(context.Context, string) error) (string, bool) {
	if !requirePost(w, r) {
		return "", false
	}

	if warm {
		// The model takes minutes to load cold and the desktop seconds to
		// start: begin loading it now so the two overlap.
		ollama.WarmAsync()
	}

	id := r.FormValue("id")

	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return "", false
	}

	if err := act(r.Context(), id); err != nil {
		jsonError(w, err)
		return "", false
	}

	return id, true
}

// startDesktopHandler starts an existing desktop by ID.
func (s *Server) startDesktopHandler(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.desktopAction(w, r, true, s.Desktops.Start); !ok {
		return
	}
	// Its host apps are kept ready again, in the background, now that it runs.
	go s.syncHostApps(context.Background())

	jsonResponse(w, map[string]string{
		"status": "started",
	})
}

// stopDesktopHandler stops a running desktop by ID.
func (s *Server) stopDesktopHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := s.desktopAction(w, r, false, s.Desktops.Stop)
	if !ok {
		return
	}

	s.closeOpenTasks(id, "The desktop was stopped before this task finished.")
	// Its host apps go too: kept ready for nobody, each with its bridge. Remembered for its next start (hostgui.go).
	s.releaseHostApps(desktop.SanitizeID(id))

	jsonResponse(w, map[string]string{
		"status": "stopped",
	})
}

// pauseDesktopHandler freezes a running desktop by ID using the cgroup freezer (0% CPU).
func (s *Server) pauseDesktopHandler(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.desktopAction(w, r, false, s.Desktops.Pause); ok {
		jsonResponse(w, map[string]string{
			"status": "paused",
		})
	}
}

// unpauseDesktopHandler thaws a paused desktop by ID (< 1ms).
func (s *Server) unpauseDesktopHandler(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.desktopAction(w, r, true, s.Desktops.Unpause); ok {
		jsonResponse(w, map[string]string{
			"status": "running",
		})
	}
}

// restartDesktopHandler restarts a desktop by ID.
func (s *Server) restartDesktopHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := s.desktopAction(w, r, true, s.Desktops.Restart)
	if !ok {
		return
	}

	s.closeOpenTasks(id, "The desktop was restarted before this task finished.")

	jsonResponse(w, map[string]string{
		"status": "restarted",
	})
}

// deleteDesktopHandler deletes a desktop and its persistent storage by ID.
func (s *Server) deleteDesktopHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := s.desktopAction(w, r, false, s.Desktops.Delete)
	if !ok {
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

// validateHostPathHandler checks whether a path on the host filesystem exists
// and returns whether it is a file or directory, its size, name, and default VM destination.
// GET or POST /api/host-paths/validate?path=...
func (s *Server) validateHostPathHandler(w http.ResponseWriter, r *http.Request) {
	hostPath := strings.TrimSpace(r.FormValue("path"))
	if hostPath == "" {
		jsonStatusError(w, http.StatusBadRequest, "path parameter is required")
		return
	}

	defaultDest := desktop.ComputeDefaultVMDest(hostPath)
	fi, err := os.Stat(hostPath)
	if err != nil {
		jsonResponse(w, map[string]any{
			"valid":        false,
			"exists":       false,
			"path":         hostPath,
			"default_dest": defaultDest,
			"error":        "Path not found on host",
		})
		return
	}

	jsonResponse(w, map[string]any{
		"valid":        true,
		"exists":       true,
		"is_dir":       fi.IsDir(),
		"name":         fi.Name(),
		"size":         fi.Size(),
		"path":         hostPath,
		"default_dest": defaultDest,
	})
}
