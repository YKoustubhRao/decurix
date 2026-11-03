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
	"github.com/YKoustubhRao/decurix/internal/withhold"
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
		internalError(w, "requests page: listing desktops", err)
		return
	}

	data := webui.PageData{
		Desktops: desktops,
	}

	if err := s.Web.RenderRequests(w, data); err != nil {
		internalError(w, "requests page", err)
	}
}

// sendRequestHandler validates and forwards a request to a specific desktop.
func (s *Server) sendRequestHandler(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}

	limitUpload(w, r)

	files, err := readAttachments(r)
	if err != nil {
		uploadError(w, err)
		return
	}

	desktopID := strings.TrimSpace(r.FormValue("id"))
	content := r.FormValue("content")

	if desktopID == "" {
		http.Error(w, "missing desktop id", http.StatusBadRequest)
		return
	}

	// Files alone are a request too: the answer to "send me the PO".
	if content == "" && len(files) == 0 {
		http.Error(w, "empty request", http.StatusBadRequest)
		return
	}

	if len(content) > maxRequestBytes {
		http.Error(w, "request too long", http.StatusBadRequest)
		return
	}

	// A reply typed under one question card answers that question only: if the desktop has since asked something
	// newer, that newer question is what the reply would land on, so it is refused (review 2026-11-02: a "yes" under
	// an old card approved a newer action).
	door := requestDoor(r)
	ctx := withDoor(s.ActorContext(r), door)
	actor, _ := actorFrom(ctx)
	origin := tasks.Origin{Kind: tasks.OriginPerson, Ref: door, ActorID: actor.ID}
	if r.FormValue("secret") == "1" {
		// The person marked this a secret: it reaches the desktop as typed and is kept nowhere as typed.
		ctx = context.WithValue(ctx, sentSecretKey{}, true)
	}
	if noteID := strings.TrimSpace(r.FormValue("note_id")); noteID != "" {
		ctx = context.WithValue(ctx, replyNoteKey{}, noteID)
	} else if seen, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(r.FormValue("seen_at"))); err == nil {
		// The desktop's chat says what was on screen: when its newest entry was posted (replyBinding).
		ctx = context.WithValue(ctx, replySeenKey{}, seen)
	}
	route, stored, err := s.submitRequestWithFiles(ctx, desktopID, content, files, origin)
	if err != nil {
		uploadError(w, err)
		return
	}

	// content is the request as the records and the chat keep it - with the files it brought named in it, and any
	// secret withheld (tasks.Store.CommitReply) - so the chat can tell its own copy from the transcript's.
	jsonResponse(w, map[string]string{
		"status":  "sent",
		"task_id": route.TaskID,
		"content": stored,
	})
}

// sentSecretKey marks a request the person sent as a secret (the compose box's toggle).
type sentSecretKey struct{}

// withheldKey marks a request or reply the records will keep withheld, whatever the reason (markWithheld).
type withheldKey struct{}

// markWithheld decides, before anything is sent, whether the records will keep content withheld (tasks.StoredForm:
// the person's mark, a question asked for a secret, or the words withhold finds), and returns ctx saying so: the
// desktop is then told "secret" (C14.1), so the two sides never disagree on what a secret is. A withheld reply, or a
// marked request, is also known to the call recorder before the desktop has it, so no model call that carries it is
// recorded as typed.
//
// What the desktop's approval gate must be told before the agent has the text is decided here too, from the same
// decision (gateNotice), and sent by deliverRequest once the desktop answers (H-PY-5, H-PY-6).
func (s *Server) markWithheld(ctx context.Context, desktopID string, route tasks.Route, content string) context.Context {
	marked, _ := ctx.Value(sentSecretKey{}).(bool)
	stored, withheld := s.Tasks.StoredForm(route, content, marked)
	notice := gateNotice{taskID: route.TaskID}
	if withheld {
		notice.values = s.rememberWithheld(desktopID, content, stored)
		ctx = context.WithValue(ctx, withheldKey{}, true)
	}
	if route.Kind != tasks.RouteReply {
		// The gate's own copy of a new request, which it keeps on its disk: the withheld form whenever anything of it
		// was withheld (a site's name is kept; a value never is), the text as sent otherwise.
		notice.copy, notice.sendCopy = content, true
		if withheld {
			notice.copy = stored
		}
	}
	return context.WithValue(ctx, gateNoticeKey{}, notice)
}

// gateNotice is what a desktop's approval gate is told, signed, before its agent has a request: its own copy of a
// new request (so whether a site is named is decided from that copy, never from a file the worker could write) and
// the values the records withheld (so the gate's own approval question withholds them).
type gateNotice struct {
	taskID   string
	copy     string
	sendCopy bool
	values   []string
}

// gateNoticeKey carries a request's gateNotice (markWithheld) to deliverRequest.
type gateNoticeKey struct{}

// tellGate gives desktopID's approval gate the notice ctx carries. It fails closed: an error means the gate does not
// know what it must, and the request is then not sent.
func (s *Server) tellGate(ctx context.Context, desktopID string) error {
	notice, ok := ctx.Value(gateNoticeKey{}).(gateNotice)
	if !ok {
		return nil
	}
	gateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 8*time.Second)
	defer cancel()
	if notice.sendCopy {
		if err := s.Desktops.SendGateRequest(gateCtx, desktopID, notice.taskID, notice.copy); err != nil {
			return err
		}
	}
	if len(notice.values) > 0 {
		if err := s.Desktops.SendGateSecrets(gateCtx, desktopID, notice.taskID, notice.values); err != nil {
			return err
		}
	}
	return nil
}

// rememberWithheld tells the call recorder the values the records withheld from content (stored is the form they
// keep): each withheld value, and the whole text only when it was withheld whole, plus any labelled value inside it.
// Never a word around a value: the recorder replaces what it is told everywhere in that desktop's calls.
func (s *Server) rememberWithheld(desktopID, content, stored string) []string {
	values := withhold.Values(content, stored)
	if stored == withhold.Secret {
		values = append(values, withhold.Values(content, withhold.Text(content))...)
	}
	s.secretsSeen.Remember(desktopID, values...)
	return values
}

// sentAsSecret reports whether what ctx carries goes to the desktop marked secret: the person marked it, or the
// records keep it withheld.
func sentAsSecret(ctx context.Context) bool {
	marked, _ := ctx.Value(sentSecretKey{}).(bool)
	withheld, _ := ctx.Value(withheldKey{}).(bool)
	return marked || withheld
}

// submitRequestWithFiles is submitLocked for a request a person attached
// files to: under the desktop's send lock the files are written into the
// desktop first, so they are there when its agent reads the request that
// names them. It returns the request as the records keep it (secrets
// withheld), which is what anything shown or stored afterwards must use.
func (s *Server) submitRequestWithFiles(ctx context.Context, desktopID, content string, files []desktop.File, origin tasks.Origin) (tasks.Route, string, error) {
	id := desktop.SanitizeID(desktopID)

	unlock := s.lockDesktop(id)
	defer unlock()

	if noteID, ok := ctx.Value(replyNoteKey{}).(string); ok && noteID != "" {
		if err := s.replyStillAnswers(id, noteID); err != nil {
			return tasks.Route{}, "", err
		}
	}

	paths, err := s.putAttachments(ctx, id, files)
	if err != nil {
		return tasks.Route{}, "", err
	}

	content = strings.TrimLeft(withAttachments(content, paths), "\n")
	if len(content) > maxRequestBytes {
		return tasks.Route{}, "", &errUpload{http.StatusBadRequest, "request too long"}
	}

	route, stored, err := s.submitStored(ctx, desktopID, content, origin)

	return route, stored, err
}

// replyNoteKey carries the question card a reply was typed under (sendRequestHandler's note_id).
type replyNoteKey struct{}

// replyStillAnswers is nil when noteID is the desktop's newest open question or approval - the one a reply now
// reaches - and an upload error (409) a person can act on otherwise. The caller holds the desktop's send lock.
func (s *Server) replyStillAnswers(desktopID, noteID string) error {
	if s.QueryNotes == nil {
		return nil
	}
	open := s.openQuestions(desktopID)
	found := false
	for _, note := range open {
		found = found || note.ID == noteID
	}
	if !found {
		return &errUpload{http.StatusConflict, "that question is already answered or closed"}
	}
	if open[0].ID != noteID {
		return &errUpload{http.StatusConflict, "this worker has asked something newer since: answer that card instead"}
	}
	return nil
}

// submitLocked sends content to a desktop through the same path every
// human request takes - Plan a task route, deliver it (starting the desktop
// first if it is stopped), then Commit the route and append it to the chat -
// and returns the route it landed under. Shared by sendRequestHandler and
// Submit, so a cron-triggered request (internal/schedule's Runner) lands in
// the task records and chat exactly the way a typed one does. The caller
// already holds the desktop's send lock (lockDesktop).
func (s *Server) submitLocked(ctx context.Context, desktopID, content string, origin tasks.Origin) (tasks.Route, error) {
	route, _, err := s.submitStored(ctx, desktopID, content, origin)
	return route, err
}

// submitStored is submitLocked, also returning the request as the records keep it.
func (s *Server) submitStored(ctx context.Context, desktopID, content string, origin tasks.Origin) (tasks.Route, string, error) {
	id := desktop.SanitizeID(desktopID)

	// Refused before anything is sent: the records would refuse to open its execution (tasks.ErrNoOrigin), and a
	// request delivered but never recorded is a run with no record.
	if origin.Kind == "" {
		return tasks.Route{}, "", tasks.ErrNoOrigin
	}

	// A desktop still being created has no container yet, which would read
	// as "no longer exists" below - say what is actually happening.
	if prov, ok := s.Desktops.GetProvisioning(id); ok && prov.Status == "provisioning" {
		return tasks.Route{}, "", errStillProvisioning
	}

	// A request to a stopped desktop waits for it to boot; load the model
	// during that wait rather than after it.
	ollama.WarmAsync()

	route := s.routeRequest(id, content, origin)

	// A reply is a person's judgement (plan R5): it needs its author, and its id is chosen now so the desktop is told
	// it with the reply (C6). No door that sends a reply lacks an actor; a reply with none is refused before delivery.
	if route.Kind == tasks.RouteReply {
		if _, ok := actorFrom(ctx); !ok {
			return tasks.Route{}, "", errNoReplier
		}
		route.JudgementID = tasks.NewJudgementID()
	}

	// Which question a reply answers, and whether it is bound to the gate's request: only then can it grant anything.
	var asked, kind, request string
	if route.Kind == tasks.RouteReply {
		var err error
		if asked, kind, request, err = s.replyBinding(ctx, id); err != nil {
			return tasks.Route{}, "", err
		}
	}

	// A reply that settles a pending approval is held to the approval policy: who may approve this action
	// (docs/access.md). Every door passes here; an automated one carries no actor and never approves under a policy.
	approval, err := s.decideApprovalReply(ctx, id, route, content)
	if err != nil {
		return tasks.Route{}, "", err
	}
	if approval != nil {
		ctx = context.WithValue(ctx, approvalKey{}, approval)
	}

	// A reply to an open question goes to the desktop's approval gate first, signed: that is the only way a yes is made.
	if route.Kind == tasks.RouteReply {
		s.signReplyToGate(ctx, id, route.TaskID, content, asked, kind, request)
	}

	ctx = s.markWithheld(ctx, id, route, content)

	// Delivered and recorded under the desktop's lifecycle lock: the idle sweep re-checks Busy under the same lock, so
	// it cannot pause or stop a desktop between the delivery and the record that makes it busy, and a stop or delete
	// waits for the record (B-23).
	unlockLifecycle := s.Desktops.LockDesktop(id)
	defer unlockLifecycle()

	if err := s.deliverRequest(ctx, desktopID, content, route); err != nil {
		return tasks.Route{}, "", err
	}

	route, stored := s.recordDelivered(ctx, id, content, route)
	return route, stored, nil
}

// Submit implements internal/schedule's Submitter: it is the fourth door
// onto submitLocked, after the chat, the requests page and a query reply -
// see that package's Runner for when this is called. Like every automated
// door it sends only to a free desktop (submitWhenFree), returning a
// *DesktopBusyError otherwise.
func (s *Server) Submit(ctx context.Context, desktopID, content string, origin tasks.Origin) error {
	_, err := s.submitWhenFree(ctx, desktopID, content, origin)
	return err
}

// SubmitTracked implements internal/dag's and internal/mailtrigger's
// Submitter: the same door Submit is, but also hands back the task ID the
// request landed under, since a DAG run has to watch that specific task to
// know when its node is done.
func (s *Server) SubmitTracked(ctx context.Context, desktopID, content string, origin tasks.Origin) (string, error) {
	route, err := s.submitWhenFree(ctx, desktopID, content, origin)
	return route.TaskID, err
}

// routeRequest decides which task record a human request belongs to before it
// is sent, because the desktop is told the task ID along with the request. A
// records failure never stops a request: it is sent as a new task regardless.
//
// origin is what started the request (plan R7): a required argument of every door, written on the execution a new
// request or a retry opens.
func (s *Server) routeRequest(desktopID, content string, origin tasks.Origin) tasks.Route {
	route, err := s.Tasks.Plan(desktopID, content)
	if err != nil {
		log.Printf("sendRequest: %v", err)
		route = tasks.NewRoute()
	}
	route.Origin = origin
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
func (s *Server) deliverRequest(ctx context.Context, desktopID, content string, route tasks.Route) error {
	// A message the records keep withheld says so to the agent too (F-2, C14): the control plane withholds it in its
	// own stores, and the agent must not keep it as typed either.
	body := desktop.RequestBody{Content: content, TaskID: route.TaskID, Secret: sentAsSecret(ctx),
		JudgementID: route.JudgementID, DateOrder: s.DateOrder}

	// A desktop with an approval gate is told what it must know first, every try, once the desktop answers: if the
	// gate cannot be told, nothing is sent (review CP-A-2 B1). A desktop made before the gate gets its request as
	// before.
	gated := s.Desktops.HasGate(ctx, desktopID)
	send := func() error {
		if gated {
			if err := s.tellGate(ctx, desktopID); err != nil {
				return err
			}
		}
		return s.Desktops.SendRequest(ctx, desktopID, body)
	}
	err := send()

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

		err = send()

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
// records and in the chat, and returns the route it was recorded under and the
// text kept: one rule for both copies (tasks.Store.CommitReply decides what of
// it is withheld; the chat keeps exactly that, C-05).
//
// Recorded only once the desktop has actually accepted it, so the chat never
// shows a request that was never delivered and no task exists for one. Every
// human-to-desktop path routes through here — the chat page, the requests
// page, and a reply to a query — so all three show up in the transcript and
// the records alike.
func (s *Server) recordDelivered(ctx context.Context, desktopID, content string, route tasks.Route) (tasks.Route, string) {
	secret, _ := ctx.Value(sentSecretKey{}).(bool)
	by := tasks.ReplyBy{Door: doorFrom(ctx)}
	by.Actor, _ = actorFrom(ctx)
	by.Approval, _ = ctx.Value(approvalKey{}).(map[string]any)
	committed, stored, err := s.Tasks.CommitReply(route, s.workerFor(ctx, desktopID), content, secret, by, route.Origin)
	if err != nil {
		log.Printf("sendRequest: %v", err)
	} else {
		route = committed
	}
	// The records kept it withheld whole (a reply to a secret question that opened after Plan picked the route):
	// the call recorder withholds it too.
	if err == nil && stored != content {
		s.rememberWithheld(desktop.SanitizeID(desktopID), content, stored)
	}

	s.Chat.AppendUserRequest(desktopID, route.TaskID, stored)

	// A request that reached the desktop as the reply to its open query is
	// the answer: the Queries tab should not go on asking for it. This is the
	// one place every door (chat, requests page, a query's own reply box)
	// passes through.
	if route.Kind == tasks.RouteReply && s.QueryNotes != nil {
		s.QueryNotes.ResolveAnswered(desktopID)
	}

	return route, stored
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

	case errors.Is(err, desktop.ErrGateNotTold):
		jsonStatusError(w, http.StatusServiceUnavailable,
			"the desktop's approval gate could not be told what was asked, so nothing was sent - try again in a moment")

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
