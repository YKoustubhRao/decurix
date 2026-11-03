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
		internalError(w, "dashboard: listing desktops", err)
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
	tasksByDesktop := map[string][]tasks.Task{}
	for _, d := range desktops {
		if linked := linkedAll[d.ID]; len(linked) > 0 {
			linkedMemory[d.ID] = linked
		}
		desktopNames[d.ID] = d.Name
		// The card's task dropdown, filled server-side so it is right on the
		// first paint; the poll keeps it current. A read error leaves the
		// dropdown to the poll rather than failing the dashboard.
		if s.Tasks != nil {
			if list, err := s.Tasks.RecentForWorker(d.ID, maxTaskPicks); err == nil && len(list) > 0 {
				tasksByDesktop[d.ID] = list
			}
		}
	}

	data := webui.PageData{
		Gated:           isGated(r),
		Desktops:        desktops,
		WorkerMemory:    memories,
		MemoryByDesktop: memoryByDesktop,
		LinkedMemory:    linkedMemory,
		DesktopNames:    desktopNames,
		TunnelURL:       s.Tunnel.Get(),
		TasksByDesktop:  tasksByDesktop,
	}

	if err := s.Web.RenderIndex(w, data); err != nil {
		internalError(w, "dashboard page", err)
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
		jsonStatusError(w, http.StatusBadRequest, refusal(brainErr))
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
	if (copyHostApps || strings.TrimSpace(r.FormValue("host_apps")) != "") && isGated(r) {
		// The host's programs and signed-in sessions are the person's to give, at their own screen - the same rule as
		// a shared folder below (review 2026-11-03, W-05): a worker made through the phone or tunnel gets none.
		jsonStatusError(w, http.StatusForbidden, "apps from this computer can only be given to a desktop on this computer, not through the phone or tunnel address")
		return
	}
	if copyHostApps {
		selectedApps = splitList(r.FormValue("host_apps"))
	}

	// Copying any file the control plane can see into a desktop was removed (review 2026-11-02): it reached the
	// control plane's own data (/data: keys, the database) and every file of the person's computer, from whoever could
	// create a desktop. The one way to give a worker files of this computer is a shared folder, chosen at its screen.
	var hostCopyPaths []desktop.HostCopyPath
	for _, field := range []string{"host_paths", "copy_host_paths", "copy_paths"} {
		if strings.TrimSpace(r.FormValue(field)) != "" && strings.TrimSpace(r.FormValue(field)) != "[]" {
			jsonStatusError(w, http.StatusBadRequest, "copying files from this computer into a desktop is no longer offered: choose a shared folder instead")
			return
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
		jsonStatusError(w, http.StatusBadRequest, refusal(sharedErr))
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

	// One lifecycle change at a time per desktop, and never in the middle of a request being delivered and recorded
	// (submitStored holds the same lock): a stop then closes what was just recorded instead of racing it (B-23).
	unlock := s.Desktops.LockDesktop(id)
	err := act(r.Context(), id)
	unlock()
	if err != nil {
		jsonError(w, err)
		return "", false
	}

	return id, true
}

// thenClose is act followed by closing the desktop's open tasks with why, both under the lifecycle lock desktopAction
// holds: a delivery waiting on that lock records its task after the close, never before it to be closed by mistake
// (RW1-E-01).
func (s *Server) thenClose(act func(context.Context, string) error, why string) func(context.Context, string) error {
	return func(ctx context.Context, id string) error {
		if err := act(ctx, id); err != nil {
			return err
		}
		s.closeOpenTasks(id, why)
		return nil
	}
}

// dockerStatusHandler answers GET /api/docker/status: whether the control plane has asked Docker yet, and whether it
// answered - what the "Docker is not running" banner reads (W-08) - and whether a tools update waits for running
// desktops to stop (tools_waiting). Never a round trip to Docker itself.
func (s *Server) dockerStatusHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	checked, reachable, toolsWaiting := false, false, false
	if s.Desktops != nil {
		checked, reachable, toolsWaiting = s.Desktops.DockerChecked(), s.Desktops.DockerReachable(), s.Desktops.ToolsRefreshWaiting(r.Context())
	}
	jsonResponse(w, map[string]bool{"checked": checked, "reachable": reachable, "tools_waiting": toolsWaiting})
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
	id, ok := s.desktopAction(w, r, false, s.thenClose(s.Desktops.Stop, "The desktop was stopped before this task finished."))
	if !ok {
		return
	}

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
	if _, ok := s.desktopAction(w, r, true, s.thenClose(s.Desktops.Restart, "The desktop was restarted before this task finished.")); !ok {
		return
	}

	jsonResponse(w, map[string]string{
		"status": "restarted",
	})
}

// deleteDesktopHandler deletes a desktop and its persistent storage by ID.
func (s *Server) deleteDesktopHandler(w http.ResponseWriter, r *http.Request) {
	// Every run not yet pulled leaves the desktop first (R9): deleting it would lose what labels those runs.
	deleteAfterFlush := func(ctx context.Context, id string) error {
		s.flushRuns(ctx, desktop.SanitizeID(id))
		if s.deleteDesktop != nil {
			return s.deleteDesktop(ctx, id)
		}
		return s.Desktops.Delete(ctx, id)
	}
	id, ok := s.desktopAction(w, r, false, s.thenClose(deleteAfterFlush, "The desktop was deleted before this task finished."))
	if !ok {
		return
	}

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
	// Its installs and roles in the org's skill registry go with it; the skills it published stay the org's.
	if s.SkillHub != nil {
		if err := s.SkillHub.ForgetWorker(desktop.SanitizeID(id)); err != nil {
			log.Printf("deleteDesktop: skill registry: %v", err)
		}
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
