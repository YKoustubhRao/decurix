package httpapi

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/ollama"
	"github.com/YKoustubhRao/decurix/internal/tasks"
	"github.com/YKoustubhRao/decurix/internal/webui"
)

// maxRequestBytes is the longest request a human can send a desktop.
const maxRequestBytes = 8192

// autoStartTimeout bounds how long deliverRequest waits for a desktop that
// internal/idlepause (or a person) had stopped to come back up and answer,
// before giving up. Generous: booting a full XFCE desktop is not instant,
// and the alternative - failing the request outright - is exactly the
// "assign it a task and it just works" promise idle-pause's default-on
// behavior depends on.
const autoStartTimeout = 90 * time.Second

// errStillProvisioning is what a request to a desktop that is still being
// created fails with.
var errStillProvisioning = errors.New("desktop is still being created")

// autoStartPoll is how often deliverRequest retries while waiting.
const autoStartPoll = time.Second

// requestsDashboardHandler renders the requests dashboard page for sending requests to desktops.
func (s *Server) requestsDashboardHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/requests" {
		http.NotFound(w, r)
		return
	}

	desktops, err := s.Desktops.List(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	data := webui.PageData{
		Desktops: desktops,
	}

	if err := s.Web.RenderRequests(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// sendRequestHandler validates and forwards a request to a specific desktop.
func (s *Server) sendRequestHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	desktopID := strings.TrimSpace(r.FormValue("id"))
	content := r.FormValue("content")

	if desktopID == "" {
		http.Error(w, "missing desktop id", http.StatusBadRequest)
		return
	}

	if content == "" {
		http.Error(w, "empty request", http.StatusBadRequest)
		return
	}

	if len(content) > maxRequestBytes {
		http.Error(w, "request too long", http.StatusBadRequest)
		return
	}

	route, err := s.submitRequest(r.Context(), desktopID, content)
	if err != nil {
		sendRequestError(w, err)
		return
	}

	jsonResponse(w, map[string]string{
		"status":  "sent",
		"task_id": route.TaskID,
	})
}

// submitRequest sends content to a desktop through the same path every
// human request takes - Plan a task route, deliver it (starting the desktop
// first if it is stopped), then Commit the route and append it to the chat -
// and returns the route it landed under. Shared by sendRequestHandler and
// Submit, so a cron-triggered request (internal/schedule's Runner) lands in
// the task records and chat exactly the way a typed one does.
func (s *Server) submitRequest(ctx context.Context, desktopID, content string) (tasks.Route, error) {
	unlock := s.lockDesktop(desktop.SanitizeID(desktopID))
	defer unlock()

	return s.submitLocked(ctx, desktopID, content)
}

// submitLocked is submitRequest for a caller already holding the desktop's
// send lock (lockDesktop).
func (s *Server) submitLocked(ctx context.Context, desktopID, content string) (tasks.Route, error) {
	id := desktop.SanitizeID(desktopID)

	// A desktop still being created has no container yet, which would read
	// as "no longer exists" below - say what is actually happening.
	if prov, ok := s.Desktops.GetProvisioning(id); ok && prov.Status == "provisioning" {
		return tasks.Route{}, errStillProvisioning
	}

	// A request to a stopped desktop waits for it to boot; load the model
	// during that wait rather than after it.
	ollama.WarmAsync()

	route := s.routeRequest(id, content)

	if err := s.deliverRequest(ctx, desktopID, content, route.TaskID); err != nil {
		return tasks.Route{}, err
	}

	return s.recordDelivered(ctx, id, content, route), nil
}

// Submit implements internal/schedule's Submitter: it is the fourth door
// onto submitRequest, after the chat, the requests page and a query reply -
// see that package's Runner for when this is called. Like every automated
// door it sends only to a free desktop (submitWhenFree), returning a
// *DesktopBusyError otherwise.
func (s *Server) Submit(ctx context.Context, desktopID, content string) error {
	_, err := s.submitWhenFree(ctx, desktopID, content)
	return err
}

// SubmitTracked implements internal/dag's and internal/mailtrigger's
// Submitter: the same door Submit is, but also hands back the task ID the
// request landed under, since a DAG run has to watch that specific task to
// know when its node is done.
func (s *Server) SubmitTracked(ctx context.Context, desktopID, content string) (string, error) {
	route, err := s.submitWhenFree(ctx, desktopID, content)
	return route.TaskID, err
}

// routeRequest decides which task record a human request belongs to before it
// is sent, because the desktop is told the task ID along with the request. A
// records failure never stops a request: it is sent as a new task regardless.
func (s *Server) routeRequest(desktopID, content string) tasks.Route {
	route, err := s.Tasks.Plan(desktopID, content)
	if err != nil {
		log.Printf("sendRequest: %v", err)
		return tasks.NewRoute()
	}

	return route
}

// deliverRequest sends content to a desktop, starting it first (and waiting
// for its agent to come back up) when it is currently stopped, instead of
// failing outright. internal/idlepause stops a desktop that has sat idle by
// default - for that to actually mean "works only when a task is assigned"
// rather than "breaks until a human notices and clicks Start", assigning it
// a task has to be able to wake it up on its own.
//
// desktop.Manager.SendRequest itself stays a fast, simple "send now, fail if
// not running" primitive; the wait-and-retry policy lives here, since both
// callers (sendRequestHandler, recordTaskHandler) are already about to
// block the human/automation on the desktop's reply anyway - there is
// nothing better to do while it boots than wait for it.
func (s *Server) deliverRequest(ctx context.Context, desktopID, content, taskID string) error {
	err := s.Desktops.SendRequest(ctx, desktopID, content, taskID)

	switch {
	case err == nil:
		return nil

	case errors.Is(err, desktop.ErrNotRunning):
		if startErr := s.Desktops.Start(ctx, desktopID); startErr != nil {
			return startErr
		}

		log.Printf("deliverRequest: %s was stopped - starting it and waiting up to %s for its agent", desktopID, autoStartTimeout)

	case errors.Is(err, desktop.ErrAgentUnreachable) && s.recentlyStarted(ctx, desktopID):
		// Running, but its agent has not bound :8081 yet - a desktop that
		// was just created, restarted or unpaused. Answering 503 here made a
		// request typed the moment a desktop turned green fail for no reason
		// a person could act on.
		log.Printf("deliverRequest: %s only just started - waiting up to %s for its agent", desktopID, autoStartTimeout)

	default:
		return err
	}

	started := time.Now()
	deadline := started.Add(autoStartTimeout)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(autoStartPoll):
		}

		err = s.Desktops.SendRequest(ctx, desktopID, content, taskID)

		switch {
		case err == nil:
			log.Printf("deliverRequest: %s was up and reachable %s after starting", desktopID, time.Since(started).Round(time.Second))
			return nil
		case !errors.Is(err, desktop.ErrNotRunning) && !errors.Is(err, desktop.ErrAgentUnreachable):
			// Something other than "still coming up" - not worth waiting out
			// the rest of the timeout for (the desktop vanished, say).
			return err
		case time.Now().After(deadline):
			log.Printf("deliverRequest: gave up waiting for %s after %s: %v", desktopID, autoStartTimeout, err)
			return err
		}
	}
}

// recentlyStarted reports whether the desktop's container started within
// autoStartTimeout, i.e. whether an agent that is not answering yet is more
// likely still booting than dead. An unreadable start time is "no": a
// desktop whose agent really is down must fail fast, not make a person wait
// a minute and a half to be told so.
func (s *Server) recentlyStarted(ctx context.Context, desktopID string) bool {
	d, err := s.Desktops.Get(ctx, desktopID)
	if err != nil {
		return false
	}

	startedAt, err := time.Parse(time.RFC3339Nano, d.StartedAt)
	if err != nil {
		return false
	}

	return time.Since(startedAt) < autoStartTimeout
}

// recordDelivered records a request the desktop has accepted, in the task
// records and in the chat, and returns the route it was recorded under.
//
// Recorded only once the desktop has actually accepted it, so the chat never
// shows a request that was never delivered and no task exists for one. Every
// human-to-desktop path routes through here — the chat page, the requests
// page, and a reply to a query — so all three show up in the transcript and
// the records alike.
func (s *Server) recordDelivered(ctx context.Context, desktopID, content string, route tasks.Route) tasks.Route {
	committed, err := s.Tasks.Commit(route, s.workerFor(ctx, desktopID), content)
	if err != nil {
		log.Printf("sendRequest: %v", err)
	} else {
		route = committed
	}

	s.Chat.AppendUserRequest(desktopID, route.TaskID, content)

	// A request that reached the desktop as the reply to its open query is
	// the answer: the Queries tab should not go on asking for it. This is the
	// one place every door (chat, requests page, a query's own reply box)
	// passes through.
	if route.Kind == tasks.RouteReply && s.QueryNotes != nil {
		s.QueryNotes.ResolveAnswered(desktopID)
	}

	return route
}

// workerFor describes a desktop for its task records: what the records already
// know, or failing that what its container says.
func (s *Server) workerFor(ctx context.Context, desktopID string) tasks.Worker {
	if known, ok := s.Tasks.Worker(desktopID); ok && known.Engine != "" && known.DeletedAt.IsZero() {
		return known
	}

	worker := tasks.Worker{ID: desktopID, Name: desktopID}

	if s.Desktops != nil {
		if found, err := s.Desktops.Get(ctx, desktopID); err == nil {
			worker.Name = found.Name
			worker.Engine = found.Harness
			worker.Profile = found.Profile
			worker.Model = found.Model
		}
	}

	return worker
}

// sendRequestError answers a failed send with the reason a human can act on,
// rather than the blanket 500 every one of these used to get. A desktop whose
// agent has not bound its port yet is the common case and is not an internal
// error: it is a "not ready yet, try again" the chat can show as-is.
func sendRequestError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errStillProvisioning):
		jsonStatusError(
			w,
			http.StatusServiceUnavailable,
			"that desktop is still being created - try again in a few seconds",
		)

	case errors.Is(err, desktop.ErrNotFound):
		jsonStatusError(
			w,
			http.StatusNotFound,
			"that desktop no longer exists",
		)

	case errors.Is(err, desktop.ErrNotRunning):
		jsonStatusError(
			w,
			http.StatusConflict,
			"that desktop is stopped - start it and try again",
		)

	case errors.Is(err, desktop.ErrAgentUnreachable):
		jsonStatusError(
			w,
			http.StatusServiceUnavailable,
			"the desktop's agent is not ready yet - it is still starting up, try again in a moment",
		)

	default:
		jsonError(w, err)
		return
	}

	log.Println("sendRequest:", err)
}
