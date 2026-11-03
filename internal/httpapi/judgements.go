package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/YKoustubhRao/decurix/internal/access"
	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// Who made a human input (plan R5, rule 5) is decided here, from the door a request came through, never from a body
// field: the signed-in user, or the local owner on the primary listener; on the gated listener without a sign-in, a
// person holding the access code (honest about what is known, and holding no role, so it approves nothing under a
// policy); from a desktop, nobody: a desktop cannot author a human judgement.

// accessCodeHolder is the actor of a reply through the gated listener by someone not signed in: they hold the
// access code (or a paired device), and nothing more is known.
var accessCodeHolder = tasks.Actor{ID: "access-code", Name: "Someone with the access code", Roles: []string{}, Kind: tasks.ActorPerson}

// actorKey carries an actor an internal door set itself (withActor).
type actorKey struct{}

// withActor returns ctx carrying a as the actor of what is sent with it: for an internal door that replies for
// someone (a worker answering through a handoff, the eval's scripted expert).
func withActor(ctx context.Context, a tasks.Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

// actorOfUser is a signed-in user (or the local owner) as the records keep an actor.
func actorOfUser(u access.User) tasks.Actor {
	roles := append([]string{}, u.Roles...)
	return tasks.Actor{ID: u.ID, Name: u.Name, Roles: roles, Kind: tasks.ActorPerson}
}

// actorFrom is the actor ctx carries (ActorContext, or withActor), and false when none: a desktop's own call, or an
// automated door.
func actorFrom(ctx context.Context) (tasks.Actor, bool) {
	if a, ok := ctx.Value(actorKey{}).(tasks.Actor); ok && a.ID != "" && a.Kind != "" {
		return a, true
	}
	if u, ok := access.ActorFrom(ctx); ok && u.ID != "" {
		return actorOfUser(u), true
	}
	if gated, _ := ctx.Value(gatedKey{}).(bool); gated && ctx.Value(personOnGatedKey{}) != nil {
		return accessCodeHolder, true
	}
	return tasks.Actor{}, false
}

// personOnGatedKey marks a context ActorContext made for a person on the gated listener who has not signed in.
type personOnGatedKey struct{}

// actorOf is the actor of r: who is acting, as ActorContext decides it.
func (s *Server) actorOf(r *http.Request) (tasks.Actor, bool) {
	return actorFrom(s.ActorContext(r))
}

// doorKey carries the door a person's request came through (chat, card, phone, concierge, ...), for its judgement.
type doorKey struct{}

// withDoor returns ctx naming the door of what is sent with it.
func withDoor(ctx context.Context, door string) context.Context {
	return context.WithValue(ctx, doorKey{}, door)
}

// doorFrom is the door ctx names, "api" when none.
func doorFrom(ctx context.Context) string {
	if d, _ := ctx.Value(doorKey{}).(string); d != "" {
		return d
	}
	return "api"
}

// requestDoor is the door of a request sent from the chat or the requests page: its form's door field when it is
// one of chat, card or phone; the card when it answers a card (note_id); the phone on the gated listener; else chat.
func requestDoor(r *http.Request) string {
	switch d := strings.TrimSpace(r.FormValue("door")); d {
	case "chat", "card", "phone":
		return d
	}
	switch {
	case strings.TrimSpace(r.FormValue("note_id")) != "":
		return "card"
	case isGated(r):
		return "phone"
	}
	return "chat"
}

// errNoReplier is a reply to a desktop's question with nobody to author it.
var errNoReplier = &errUpload{http.StatusForbidden, "a desktop cannot answer its own question"}

// approvalKey carries the access log's decision on an approval reply, for the approval judgement's payload.
type approvalKey struct{}

// judgementsHandler lists judgements (?task=<id> or ?worker=<id>), oldest first; a deleted worker's are served.
// Primary listener, humansOnly.
func (s *Server) judgementsHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	q := r.URL.Query()
	task, worker := strings.TrimSpace(q.Get("task")), strings.TrimSpace(q.Get("worker"))
	var list []tasks.Judgement
	var err error
	switch {
	case task != "" && tasks.ValidID(task):
		list, err = s.Tasks.JudgementsForTask(task)
	case worker != "":
		list, err = s.Tasks.JudgementsForWorker(desktop.SanitizeID(worker))
	default:
		jsonStatusError(w, http.StatusBadRequest, "name a task (?task=) or a worker (?worker=)")
		return
	}
	if err != nil {
		internalError(w, "judgements", err)
		return
	}
	jsonResponse(w, map[string]any{"judgements": list})
}
