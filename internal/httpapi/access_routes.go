package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"slices"
	"strings"

	"github.com/YKoustubhRao/decurix/internal/access"
	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/querynotes"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// accessHandlers holds each Server's access handler. It lives here rather
// than as a Server field only so this pillar did not touch server.go; moving
// it to a field (Server.Access *access.Handler) later changes nothing else.
// SetAccess wires users, roles and approval policies into the server. Without
// it every reply settles an approval as before.
func (s *Server) SetAccess(store *access.Store) {
	if store == nil {
		s.Access = nil
		return
	}
	s.Access = &access.Handler{Store: store, Middleware: access.Middleware{Store: store, Gated: isGated}}
}

// access returns the server's access handler, or nil when none is wired.
func (s *Server) access() *access.Handler {
	return s.Access
}

// RegisterAccessRoutes registers sign-in (/api/auth/login, logout, me) on mux;
// call it for both listeners. On the primary one they report the local owner.
func (s *Server) RegisterAccessRoutes(mux *http.ServeMux) {
	if h := s.access(); h != nil {
		h.RegisterAuth(mux)
	}
}

// RegisterAccessLocalRoutes registers the users, roles, policies and
// approvals-log routes; primary listener only, for a person, never a desktop.
func (s *Server) RegisterAccessLocalRoutes(mux *http.ServeMux) {
	if h := s.access(); h != nil {
		h.RegisterAdmin(mux, s.humansOnly)
	}
}

// ActorContext returns r's context carrying who is acting: the local owner on
// the primary listener, the signed-in user on the gated one, and nobody for
// a desktop's own call (an agent must never approve its own action). With no
// access store wired, the primary listener's person is still the local owner.
// A person on the gated listener who has not signed in is marked as such: the
// records name them as holding the access code (actorFrom), and no policy
// lets them approve.
func (s *Server) ActorContext(r *http.Request) context.Context {
	ctx := r.Context()
	if s.fromContainer(r) {
		return ctx
	}
	if h := s.access(); h != nil {
		if u, ok := h.Middleware.Actor(r); ok {
			return access.WithActor(ctx, u)
		}
	} else if !isGated(r) {
		return access.WithActor(ctx, access.LocalOwner)
	}
	if isGated(r) {
		return context.WithValue(ctx, personOnGatedKey{}, true)
	}
	return ctx
}

// ApprovalRefusedError is a reply to a pending approval that was not delivered.
type ApprovalRefusedError struct {
	// Status is 403 when the person may not approve, 409 when their yes is held for a second approver (not 2xx: the chat must not show the reply as sent).
	Status  int
	Message string
}

// Error returns the plain message for the person.
func (e *ApprovalRefusedError) Error() string { return e.Message }

// writeApprovalRefused answers err if it is an ApprovalRefusedError and reports whether it did.
func writeApprovalRefused(w http.ResponseWriter, err error) bool {
	var refused *ApprovalRefusedError
	if !errors.As(err, &refused) {
		return false
	}
	jsonResponseStatus(w, refused.Status, map[string]string{"error": refused.Message, "status": statusWord(refused.Status)})
	return true
}

func statusWord(status int) string {
	if status == http.StatusConflict {
		return "held"
	}
	return "refused"
}

// jsonResponseStatus writes v as JSON with status.
func jsonResponseStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// gateApprovalReply decides a request that is about to reach a desktop as
// the reply to its pending approval: nil lets it through, an
// *ApprovalRefusedError stops it before delivery. It does nothing when no
// access store is wired, when the request is not a reply, or when what the
// desktop waits on is a question rather than an approval. A failure to read
// the policies refuses (fails closed).
func (s *Server) gateApprovalReply(ctx context.Context, desktopID string, route tasks.Route, content string) error {
	_, err := s.decideApprovalReply(ctx, desktopID, route, content)
	return err
}

// decideApprovalReply is gateApprovalReply, also returning what the approvals log recorded of an approval reply
// (approval_id, outcome, decision) for its judgement; nil when no approval was decided.
func (s *Server) decideApprovalReply(ctx context.Context, desktopID string, route tasks.Route, content string) (map[string]any, error) {
	h := s.access()
	if h == nil || route.Kind != tasks.RouteReply {
		return nil, nil
	}

	// The task's own open intervention first: every approval the agent asks is recorded there, whether or not a note
	// was shown (a quiet approval, an older image's untyped one); the desktop's latest note is the fallback.
	action, ok := s.openApproval(route.TaskID)
	if !ok {
		action, ok = s.pendingApproval(desktop.SanitizeID(desktopID))
	}
	if !ok {
		return nil, nil
	}

	actor, _ := access.ActorFrom(ctx) // an automated door carries none: it may not approve under a policy
	decision, err := h.Store.Decide(actor, desktop.SanitizeID(desktopID), s.workerRoles(desktop.SanitizeID(desktopID)), route.TaskID, action, content)
	if err != nil {
		log.Printf("approval gate for %s: %v", desktopID, err)
		return nil, &ApprovalRefusedError{Status: http.StatusServiceUnavailable, Message: "The approval rules could not be read, so this reply was not sent. Try again."}
	}
	if decision.Allowed {
		return map[string]any{"approval_id": decision.ApprovalID, "outcome": decision.Outcome,
			"decision": decision.RecordedAs}, nil
	}

	status := http.StatusForbidden
	if decision.Outcome == access.OutcomeHeld {
		status = http.StatusConflict
	}
	return nil, &ApprovalRefusedError{Status: status, Message: decision.Message}
}

// pendingApproval returns the text of the approval desktopID is waiting on:
// its latest note, when that note is an approval not yet answered (a note a
// person only closed by hand is still waiting).
func (s *Server) pendingApproval(desktopID string) (string, bool) {
	if s.QueryNotes == nil {
		return "", false
	}

	var latest *querynotes.QueryNote
	consider := func(notes []querynotes.QueryNote) {
		for i := range notes {
			n := notes[i]
			if n.DesktopID != desktopID || (n.Kind != querynotes.KindApproval && n.Kind != querynotes.KindQuery) {
				continue
			}
			if latest == nil || n.CreatedAt.After(latest.CreatedAt) {
				latest = &n
			}
		}
	}
	consider(s.QueryNotes.List())
	consider(s.QueryNotes.ListResolved())

	if latest == nil || latest.Kind != querynotes.KindApproval {
		return "", false
	}
	if latest.ResolvedAt != nil && latest.Resolution != querynotes.ResolutionManual {
		return "", false
	}
	return latest.Content, true
}

// openApproval returns the request of the approval the task is waiting on - the latest attempt's intervention of kind
// approval that has no reply yet - from the task record itself; false when there is none or no task.
func (s *Server) openApproval(taskID string) (string, bool) {
	if s.Tasks == nil || taskID == "" {
		return "", false
	}
	d, err := s.Tasks.Get(taskID)
	if err != nil || len(d.Executions) == 0 {
		return "", false
	}
	e := d.Executions[len(d.Executions)-1]
	for i := len(e.Interventions) - 1; i >= 0; i-- {
		iv := e.Interventions[i]
		if iv.RepliedAt.IsZero() && strings.EqualFold(iv.Kind, "approval") {
			return iv.Request, true
		}
	}
	return "", false
}

// workerRoles is every role a worker carries: those the skill registry gives it (what it is taught for) and those the
// access store gives it (what its approvals are held to) - one set, so a role means the same in both places. nil when
// neither store knows the worker (access then reads its own).
func (s *Server) workerRoles(worker string) []string {
	var roles []string
	if s.SkillHub != nil {
		if rs, err := s.SkillHub.WorkerRoles(worker); err == nil {
			roles = append(roles, rs...)
		}
	}
	if h := s.access(); h != nil {
		roles = append(roles, h.Store.WorkerRoles(worker)...)
	}
	if len(roles) == 0 {
		return nil
	}
	slices.Sort(roles)
	return slices.Compact(roles)
}
