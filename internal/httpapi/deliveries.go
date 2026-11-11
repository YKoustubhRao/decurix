// Requests and messages that survive a restart of the control plane (internal/delivery).
//
// A request is written down before it is sent and settled once the task records hold it, so a crash between the
// two is found again when the process starts: ReconcileDeliveries asks the desktop whether it has the request, and
// records it, tells the person to send it again, or keeps it unconfirmed so the desktop's late outcome for it is
// accepted rather than refused. And a desktop's message (a question, an approval, an outcome) is applied once per
// event id, however many times the desktop resends it until it is acknowledged.
package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// processStarted is when this control plane started: a send still marked "sending" from before it was cut short.
var processStarted = time.Now()

// sendingRecord is what the control plane keeps of a request while it is sent: everything CommitStored needs to
// record it later, with the request only in the form the records keep it (never as typed).
type sendingRecord struct {
	Route    tasks.Route    `json:"route"`
	Worker   tasks.Worker   `json:"worker"`
	Stored   string         `json:"stored"`
	Withheld bool           `json:"withheld"`
	Door     string         `json:"door,omitempty"`
	Actor    tasks.Actor    `json:"actor"`
	Approval map[string]any `json:"approval,omitempty"`
}

// beginSending writes a request down before it is sent, returning the row to settle (0 when none was written: a
// records failure never stops a request, as routeRequest).
func (s *Server) beginSending(ctx context.Context, desktopID, content string, route tasks.Route) int64 {
	if s.Deliveries == nil {
		return 0
	}
	secret, _ := ctx.Value(sentSecretKey{}).(bool)
	stored, withheld := tasks.RequestForm(content, secret)
	record := sendingRecord{Route: route, Worker: s.workerFor(ctx, desktopID), Stored: stored, Withheld: withheld,
		Door: doorFrom(ctx)}
	record.Actor, _ = actorFrom(ctx)
	record.Approval, _ = ctx.Value(approvalKey{}).(map[string]any)
	payload, err := json.Marshal(record)
	if err != nil {
		log.Printf("deliveries: %s: %v", desktopID, err)
		return 0
	}
	id, err := s.Deliveries.Begin(desktop.SanitizeID(desktopID), route.TaskID, payload)
	if err != nil {
		log.Printf("deliveries: %s: %v", desktopID, err)
		return 0
	}
	return id
}

// settleSending removes a request's row once it is recorded or was never delivered.
func (s *Server) settleSending(id int64) {
	if s.Deliveries == nil || id == 0 {
		return
	}
	if err := s.Deliveries.Done(id); err != nil {
		log.Printf("deliveries: %v", err)
	}
}

// recordStored records a request from what was kept of it before it was sent, as recordDelivered records one just
// delivered: in the task records and in the chat, resolving the question a reply answers.
func (s *Server) recordStored(desktopID string, record sendingRecord) (tasks.Route, error) {
	route, stored, err := s.Tasks.CommitStored(record.Route, record.Worker, record.Stored, record.Withheld,
		tasks.ReplyBy{Actor: record.Actor, Door: record.Door, Approval: record.Approval}, record.Route.Origin)
	if err != nil {
		return route, err
	}
	s.Chat.AppendUserRequest(desktopID, route.TaskID, stored)
	if route.Kind == tasks.RouteReply && s.QueryNotes != nil {
		s.QueryNotes.ResolveAnswered(desktopID)
	}
	return route, nil
}

// receivedTimeout bounds asking one desktop whether it has a request.
const receivedTimeout = 10 * time.Second

// askReceived asks a desktop's agent whether a request for taskID arrived since sending began (agent GET
// /requests/<task>?since=<epoch seconds>, receipts.py): since, because a reply or a retry carries the task id of the
// request it continues. known is false when it cannot say: stopped, unreachable, past what it keeps, or an agent
// from before it could be asked.
func (s *Server) askReceived(ctx context.Context, desktopID, taskID string, since time.Time) (received, known bool) {
	if s.Desktops == nil {
		return false, false
	}
	ctx, cancel := context.WithTimeout(ctx, receivedTimeout)
	defer cancel()
	path := "/requests/" + url.PathEscape(taskID) + "?since=" + strconv.FormatInt(since.Unix(), 10)
	resp, err := s.Desktops.AgentRequest(ctx, desktopID, http.MethodGet, path, nil)
	if err != nil {
		return false, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, false
	}
	var answer struct {
		Received *bool `json:"received"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&answer); err != nil || answer.Received == nil {
		return false, false
	}
	return *answer.Received, true
}

// ReconcileDeliveries settles every request a crash cut short between its delivery and its record: one the records
// hold already is done; one its desktop has is recorded now; one its desktop says it never got is dropped and the
// person told to send it again; and one whose desktop cannot say is kept unconfirmed, so its late outcome is
// accepted, and the person is told it may not have arrived. Run once Docker answers (cmd/decurix dockerStartup).
func (s *Server) ReconcileDeliveries(ctx context.Context) {
	if s.Deliveries == nil {
		return
	}
	if err := s.Deliveries.Prune(); err != nil {
		log.Printf("deliveries: %v", err)
	}
	left, err := s.Deliveries.Leftovers(processStarted)
	if err != nil {
		log.Printf("deliveries: %v", err)
		return
	}
	for _, item := range left {
		s.reconcileOne(ctx, item.ID, item.DesktopID, item.TaskID, item.Payload, item.StartedAt)
	}
}

// reconcileOne settles one cut-short send (ReconcileDeliveries).
func (s *Server) reconcileOne(ctx context.Context, id int64, desktopID, taskID string, payload []byte, started time.Time) {
	var record sendingRecord
	if err := json.Unmarshal(payload, &record); err != nil {
		log.Printf("deliveries: send %d to %s is unreadable (%v); dropped", id, desktopID, err)
		s.settleSending(id)
		return
	}
	if done, err := s.Tasks.Committed(record.Route); err == nil && done {
		s.settleSending(id)
		return
	}

	when := started.Local().Format("15:04")
	ask := s.askReceived
	if s.askReceivedFn != nil {
		ask = s.askReceivedFn
	}
	received, known := ask(ctx, desktopID, taskID, started)
	// A desktop the cut-short send had just started is often still booting when the control plane is back: its agent
	// is asked again until it answers or the boot wait is over (found live 2026-11-11: asked at :35, listening at :34
	// of the next second), rather than settling for "cannot say" at once.
	for deadline := time.Now().Add(autoStartTimeout); !known && time.Now().Before(deadline) && s.booting(ctx, desktopID); {
		select {
		case <-ctx.Done():
			return
		case <-time.After(reconcileRetry):
		}
		received, known = ask(ctx, desktopID, taskID, started)
	}
	switch {
	case known && received:
		if _, err := s.recordStored(desktopID, record); err != nil {
			log.Printf("deliveries: recording %s's request %s: %v", desktopID, taskID, err)
			return
		}
		log.Printf("deliveries: %s had request %s, sent as the control plane stopped; recorded now", desktopID, taskID)
		s.settleSending(id)
	case known:
		log.Printf("deliveries: %s never got request %s (the control plane stopped while sending it)", desktopID, taskID)
		s.Chat.AppendAgentNote(desktopID, "", "error", fmt.Sprintf(
			"A request sent at %s did not reach this worker: the control plane stopped while sending it. "+
				"Nothing was done; send it again.", when))
		s.settleSending(id)
	default:
		if err := s.Deliveries.MarkUnconfirmed(id); err != nil {
			log.Printf("deliveries: %v", err)
			return
		}
		log.Printf("deliveries: %s could not say whether it has request %s; kept unconfirmed", desktopID, taskID)
		s.Chat.AppendAgentNote(desktopID, "", "error", fmt.Sprintf(
			"A request sent at %s may not have reached this worker: the control plane stopped while sending it, "+
				"and the worker could not be asked. If it answers, the answer shows here; look at its screen before "+
				"sending it again.", when))
	}
}

// reconcileRetry is how often a booting desktop's agent is asked again (reconcileOne).
var reconcileRetry = 3 * time.Second

// booting reports whether a desktop's agent not answering is likelier its boot than its absence: the desktop runs and
// started within autoStartTimeout (recentlyStarted). Tests stand in with bootingFn.
func (s *Server) booting(ctx context.Context, desktopID string) bool {
	if s.bootingFn != nil {
		return s.bootingFn(desktopID)
	}
	if s.Desktops == nil {
		return false
	}
	return s.recentlyStarted(ctx, desktopID)
}

// acceptUnconfirmed records the cut-short request of taskID now that its desktop answered it, so the outcome is
// recorded rather than refused as a task nobody opened. It reports whether there was one.
func (s *Server) acceptUnconfirmed(desktopID, taskID string) bool {
	if s.Deliveries == nil || taskID == "" {
		return false
	}
	item, ok, err := s.Deliveries.TakeUnconfirmed(desktopID, taskID)
	if err != nil {
		log.Printf("deliveries: %v", err)
	}
	if !ok {
		return false
	}
	var record sendingRecord
	if err := json.Unmarshal(item.Payload, &record); err != nil {
		log.Printf("deliveries: unconfirmed send %d is unreadable: %v", item.ID, err)
		return false
	}
	if _, err := s.recordStored(desktopID, record); err != nil {
		log.Printf("deliveries: recording %s's request %s: %v", desktopID, taskID, err)
		return false
	}
	log.Printf("deliveries: %s answered request %s, unconfirmed since a restart; recorded now", desktopID, taskID)
	return true
}

// eventIDPattern is an event id a desktop may send: hex, as the agent makes them (uuid4 hex). Anything else is
// treated as none (the message is always applied).
var eventIDPattern = regexp.MustCompile(`^[0-9a-f]{8,64}$`)

// applyOnce runs apply for a desktop's message unless the same event id was applied already, reporting whether it
// was a duplicate.
func (s *Server) applyOnce(desktopID, eventID string, apply func() bool) bool {
	if s.Deliveries == nil || !eventIDPattern.MatchString(eventID) {
		apply()
		return false
	}
	duplicate, err := s.Deliveries.Apply(desktopID, eventID, apply)
	if err != nil {
		log.Printf("deliveries: %v", err)
	}
	return duplicate
}
