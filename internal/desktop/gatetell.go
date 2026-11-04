package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/docker/docker/api/types/container"
)

// What a desktop's approval gate is told before its agent has a text, and in what order, is decided here once for
// every path that gives an agent a request or a reply: the control plane's own doors (httpapi.deliverRequest,
// sendTaught) and the practice and eval runners (PracticeAgentRequest). So an eval measures the gate production runs,
// and the two can never drift (docs/gate-boundary.md).

// GateReply is a reply as its desktop's gate is given it (/reply): its text, the question it answers, that
// question's kind ("approval" or "query") and the gate's request it answers ("" for any other question).
type GateReply struct{ Text, Asked, Kind, RequestID string }

// GateNotice is what a desktop's approval gate is told before its agent has a text sent on a task, from the C14
// decision on that text (tasks.Store.StoredForm, or tasks.RequestForm / tasks.ReplyForm where no route is kept).
type GateNotice struct {
	TaskID string
	// Withheld is the records keeping the text withheld: the agent is then told "secret" too (C14.1).
	Withheld bool
	// Values are the values withheld (/secrets): the gate withholds them from the questions it words.
	Values []string
	// Copy is the gate's own copy of a new request (/request), sent when SendCopy.
	Copy     string
	SendCopy bool
	// Reply is a reply to give the gate (/reply), nil for a new request or a reply bound to nothing.
	Reply *GateReply
}

// RequestNotice is the notice for a new request on taskID: the gate gets its copy, the withheld form (stored)
// whenever anything of it was withheld (a site's name is kept, a value never is), and the values withheld.
func RequestNotice(taskID, content, stored string, withheld bool, values []string) GateNotice {
	n := GateNotice{TaskID: taskID, Withheld: withheld, Copy: content, SendCopy: true}
	if withheld {
		n.Copy, n.Values = stored, values
	}
	return n
}

// ReplyNotice is the notice for a reply on taskID: never a request copy, the values withheld, and reply (nil when the
// reply is bound to no question the gate can settle).
func ReplyNotice(taskID string, withheld bool, values []string, reply *GateReply) GateNotice {
	n := GateNotice{TaskID: taskID, Withheld: withheld, Reply: reply}
	if withheld {
		n.Values = values
	}
	return n
}

// TellGate gives desktop id's approval gate n in production's order, failing closed: the values (/secrets) first,
// then the gate's copy of a new request (/request) or the reply (/reply). A gate that does not take the values or the
// copy is an error, and the caller then gives the agent nothing. A /reply the gate does not take is only logged and
// the reply is still delivered: such a reply grants nothing (the gate alone makes a yes, from a /reply it verified).
//
// The one exception is a practice desktop made for pinned old code (PracticeSpec.LegacyGate, set by an eval's
// old-code arm alone): its gate predates /secrets and /request, so a 404 from those is logged and passed. Every other
// failure there fails closed too, and no other desktop has the exception.
func (m *Manager) TellGate(ctx context.Context, id string, n GateNotice) error {
	legacy := m.legacyPracticeGate(ctx, id)
	pass := func(path string, err error) error {
		if err != nil && legacy && errors.Is(err, ErrGateNoRoute) {
			log.Printf("desktop %s: its gate (pinned old code, practice only) has no %s; delivering without it", id, path)
			return nil
		}
		return err
	}
	// The desktop's screen password goes with every task's values (screenheld.go), so the gate withholds it from the
	// questions it words. With no value of the task's own it is only a second wall: a gate that does not take it
	// (one older than /secrets, or not up yet) is logged and the notice goes on as it did before.
	screen := ScreenHeld(id)
	if values := append(append([]string(nil), n.Values...), screen...); len(values) > 0 {
		err := m.SendGateSecrets(ctx, id, n.TaskID, values)
		if err != nil && len(n.Values) == 0 {
			log.Printf("desktop %s: its gate did not take the screen password: %v", id, err)
			err = nil
		}
		if err := pass("/secrets", err); err != nil {
			return err
		}
	}
	if n.SendCopy {
		if err := pass("/request", m.SendGateRequest(ctx, id, n.TaskID, n.Copy)); err != nil {
			return err
		}
	}
	if n.Reply != nil {
		r := n.Reply
		if err := m.SendGateReply(ctx, id, n.TaskID, r.Text, r.Asked, r.Kind, r.RequestID); err != nil {
			log.Printf("reply to %s: not given to its approval gate: %v", id, err)
		}
	}
	return nil
}

// ErrGateNoRoute is a gate that answered 404 for what it was given: a gate older than that route.
var ErrGateNoRoute = errors.New("the approval gate does not know this route")

// gateNoticeKey carries a GateNotice to PracticeAgentRequest (WithGateNotice).
type gateNoticeKey struct{}

// WithGateNotice returns ctx carrying n, the notice PracticeAgentRequest gives a practice desktop's gate before it
// posts a /request to its agent.
func WithGateNotice(ctx context.Context, n GateNotice) context.Context {
	return context.WithValue(ctx, gateNoticeKey{}, n)
}

// GateNoticeFrom is the notice ctx carries (WithGateNotice), and whether it carries one.
func GateNoticeFrom(ctx context.Context) (GateNotice, bool) {
	n, ok := ctx.Value(gateNoticeKey{}).(GateNotice)
	return n, ok
}

// errNoGateNotice is a practice /request with no notice for its gate: refused, as production refuses a request whose
// gate was not told.
var errNoGateNotice = fmt.Errorf("%w: a practice request carried nothing to tell its gate", ErrGateNotTold)

// tellPracticeGate is PracticeAgentRequest's gate step for a POST /request: the notice ctx carries, for the task the
// body names, given to the practice desktop's gate by TellGate; and "secret" set in the body when the records keep the
// text withheld, as production tells the agent. It returns the body to send.
func (m *Manager) tellPracticeGate(ctx context.Context, d PracticeDesktop, body io.Reader) (io.Reader, error) {
	n, ok := ctx.Value(gateNoticeKey{}).(GateNotice)
	if !ok {
		return nil, errNoGateNotice
	}
	var raw []byte
	if body != nil {
		var err error
		if raw, err = io.ReadAll(io.LimitReader(body, 16<<20)); err != nil {
			return nil, err
		}
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, fmt.Errorf("a practice request that is not a JSON object: %w", ErrGateNotTold)
	}
	if task, _ := fields["task_id"].(string); task == "" || task != n.TaskID {
		return nil, fmt.Errorf("%w: the notice is for task %q, the request for %q", ErrGateNotTold, n.TaskID, task)
	}
	if err := m.TellGate(withPracticeTarget(ctx, d.ContainerID), d.Name, n); err != nil {
		return nil, err
	}
	if n.Withheld {
		fields["secret"] = true
		if raw, _ = json.Marshal(fields); raw == nil {
			return nil, ErrGateNotTold
		}
	}
	return bytes.NewReader(raw), nil
}

// practiceTargetKey carries the practice container a gate post is for (withPracticeTarget).
type practiceTargetKey struct{}

// withPracticeTarget returns ctx saying its gate posts are for the practice container containerID: what the practice
// caller (PracticeAgentRequest, or a practice task's end) opts into. Nothing infers it from a desktop's name.
func withPracticeTarget(ctx context.Context, containerID string) context.Context {
	return context.WithValue(ctx, practiceTargetKey{}, containerID)
}

// practiceContainer is the container CreatePractice made for practice desktop name, "" for any other name.
func (m *Manager) practiceContainer(name string) string {
	m.legacyMu.Lock()
	defer m.legacyMu.Unlock()
	return m.practiceDesks[sanitizeName(name)]
}

// ErrReservedName is a desktop name kept for the control plane's own practice and readiness workers.
var ErrReservedName = errors.New("names starting with practice- or readiness- are kept for practice and readiness runs")

// ReservedName reports whether a desktop name is one practice or readiness runs name their workers by: refused at
// every create (BeginProvisioning), so a person's desktop never shares a practice desktop's id, gate key or gate.
func ReservedName(name string) bool {
	id := sanitizeName(name)
	return strings.HasPrefix(id, "practice-") || strings.HasPrefix(id, "readiness-")
}

// gateContainer is desktop id's container as the gate is reached in it: desktop_<id>, or, when ctx opts into a
// practice container (withPracticeTarget), that container, which must carry PracticeLabel, be no mock app and bear
// id as its name.
func (m *Manager) gateContainer(ctx context.Context, id string) (container.InspectResponse, string, error) {
	desktopID := sanitizeName(id)
	if cid, ok := ctx.Value(practiceTargetKey{}).(string); ok {
		practice, err := m.client.ContainerInspect(ctx, cid)
		if err != nil {
			return practice, desktopID, err
		}
		if practice.Config == nil || practice.Config.Labels[PracticeLabel] != "true" || practice.Config.Labels["decurix-practice-mock"] == "true" ||
			practice.ContainerJSONBase == nil || strings.TrimPrefix(practice.Name, "/") != desktopID {
			return practice, desktopID, fmt.Errorf("container %s is not practice desktop %s", cid, desktopID)
		}
		return practice, desktopID, nil
	}
	name := "desktop_" + desktopID
	inspect, err := m.client.ContainerInspect(ctx, name)
	return inspect, name, err
}

// legacyPracticeGate reports whether this gate post is for a practice container CreatePractice made with LegacyGate:
// the practice caller's own container (withPracticeTarget), known by the ID this control plane recorded at create,
// and still that practice desktop. Never by a name or anything on a container.
func (m *Manager) legacyPracticeGate(ctx context.Context, id string) bool {
	cid, ok := ctx.Value(practiceTargetKey{}).(string)
	if !ok {
		return false
	}
	m.legacyMu.Lock()
	recorded := m.legacyGates[cid]
	m.legacyMu.Unlock()
	if !recorded {
		return false
	}
	_, _, err := m.gateContainer(ctx, id)
	return err == nil
}

// wake unpauses container name. One that is no longer paused (another caller woke it first: Docker answers "is not
// paused") and is running counts as woken.
func (m *Manager) wake(ctx context.Context, name string) error {
	err := m.client.ContainerUnpause(ctx, name)
	if err == nil {
		return nil
	}
	if inspect, ierr := m.client.ContainerInspect(ctx, name); ierr == nil && inspect.State != nil && inspect.State.Running && !inspect.State.Paused {
		return nil
	}
	return err
}
