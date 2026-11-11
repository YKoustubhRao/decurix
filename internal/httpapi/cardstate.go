// A desktop card's one status, derived when it is read from the facts the control plane keeps, never stored.
//
// The card used to work its pill out in the page's script from three sources side by side: Docker's status, the
// chat's "a turn is running" flag and the open Query notes. When those disagreed the card did too, most visibly as
// "Working..." for good after the records had closed the task. Here the same rules run in one place, with the task
// records as the tie-breaker, and a turn nothing has been heard from for a long while reads "no signal" rather than
// a confident "working" (after agent-orchestrator's derived display status).
package httpapi

import (
	"fmt"
	"time"
)

// Card states, in the classes the dashboard's pill already has.
const (
	CardRunning   = "running"
	CardPaused    = "paused"
	CardStopped   = "stopped"
	CardCrashed   = "crashed"
	CardNeedsYou  = "needs-you"
	CardRecording = "recording"
	CardWorking   = "working"
	CardNoSignal  = "no-signal"
)

// staleTurnAfter is how long a running chat turn the task records hold no live execution for is still believed: a
// desktop's steps and its reply travel separately, and a desktop may report work it started on its own.
const staleTurnAfter = 2 * time.Minute

// noSignalAfter is how long a working desktop may go without a step before its card says nothing has been heard.
const noSignalAfter = 10 * time.Minute

// cardFacts is what a card's state is derived from.
type cardFacts struct {
	// Status and Crash are Docker's (desktop.Desktop): running, paused, stopped...; why it stopped on its own.
	Status string
	Crash  string
	// TurnRunning is the chat's "a turn is running"; Ticket says the turn is a person recording ("/record").
	TurnRunning bool
	Ticket      bool
	// LastSignal is when the desktop was last heard from in this turn: its latest step, or the turn's start.
	LastSignal time.Time
	// Live says the task records hold a queued, running or waiting execution for this desktop.
	Live bool
	// Waiting says an open Query note waits on a person (a question or an approval); Reported that only a finished
	// task's report is open.
	Waiting  bool
	Reported bool
}

// CardState is a card's status: its pill class, the word on it and, when there is more to say, why.
type CardState struct {
	State string `json:"state"`
	Label string `json:"label"`
	Why   string `json:"why,omitempty"`
}

// deriveCardState is the card's status from its facts, by the rules the dashboard's pill has followed (index.html
// paintPills): a crash or a stop, unless a turn proves it up again; then needs-you, recording, working, running or
// paused, in that order. Two additions: a turn counts as work only while the records agree or it was heard from
// lately, and work nothing has been heard from in noSignalAfter reads no-signal.
func deriveCardState(f cardFacts, now time.Time) CardState {
	quiet := now.Sub(f.LastSignal)
	turn := f.TurnRunning && (f.Live || quiet < staleTurnAfter)

	if f.Crash != "" && !turn {
		return CardState{State: CardCrashed, Label: "crashed", Why: "Crashed: " + f.Crash}
	}
	base := f.Status
	if base == CardStopped && !turn {
		return CardState{State: CardStopped, Label: "stopped"}
	}
	// A turn in progress means the desktop is up again, whatever the (cached) list last said.
	if base == CardStopped || f.Crash != "" {
		base = CardRunning
	}
	if base != CardRunning && base != CardPaused {
		return CardState{State: base, Label: base}
	}

	live := base == CardRunning || turn
	switch {
	case live && (f.Waiting || (f.Reported && !turn)):
		return CardState{State: CardNeedsYou, Label: "needs you"}
	case turn && f.Ticket:
		return CardState{State: CardRecording, Label: "recording"}
	case turn && quiet >= noSignalAfter:
		return CardState{State: CardNoSignal, Label: "no signal", Why: fmt.Sprintf(
			"Nothing heard from this worker for %d minutes. It may still be working on a long step; open it to look.",
			int(quiet.Minutes()))}
	case turn:
		return CardState{State: CardWorking, Label: "working"}
	case live:
		return CardState{State: CardRunning, Label: "running"}
	default:
		return CardState{State: CardPaused, Label: "paused"}
	}
}

// cardAttention is, per desktop, whether an open Query note waits on a person and whether only finished reports are
// open (the rail's own split, index.html waitsOnYou).
func (s *Server) cardAttention() (waiting, reported map[string]bool) {
	waiting, reported = map[string]bool{}, map[string]bool{}
	if s.QueryNotes == nil {
		return waiting, reported
	}
	for _, note := range s.QueryNotes.List() {
		if note.DesktopID == "" {
			continue
		}
		if note.Kind == "" || note.Kind == "query" || note.Kind == "approval" {
			waiting[note.DesktopID] = true
		} else {
			reported[note.DesktopID] = true
		}
	}
	return waiting, reported
}

// liveWorkers is the set of desktops the task records hold a live execution for; nil when it cannot be read, which
// leaves every running turn believed (the card's old rule).
func (s *Server) liveWorkers() map[string]bool {
	if s.Tasks == nil {
		return nil
	}
	ids, err := s.Tasks.LiveWorkers()
	if err != nil {
		return nil
	}
	live := make(map[string]bool, len(ids))
	for _, id := range ids {
		live[id] = true
	}
	return live
}

// cardStateFor derives one card's state from what a poll already read: its activity (chat and Docker), the open
// notes and the live executions (nil: unknown, every running turn believed).
func cardStateFor(id string, a Activity, signal time.Time, waiting, reported, live map[string]bool, now time.Time) CardState {
	return deriveCardState(cardFacts{
		Status: a.Status, Crash: a.Crash, TurnRunning: a.Running, Ticket: a.Ticket, LastSignal: signal,
		Live: live == nil || live[id], Waiting: waiting[id], Reported: reported[id],
	}, now)
}
