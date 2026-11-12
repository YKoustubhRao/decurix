// Tests for a card's derived status (cardstate.go): the dashboard's pill rules, with the task records settling a turn
// the chat still calls running and a long-quiet turn reading "no signal".
package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDeriveCardState(t *testing.T) {
	now := time.Now()
	recent, quiet, silent := now.Add(-30*time.Second), now.Add(-5*time.Minute), now.Add(-15*time.Minute)

	cases := []struct {
		name  string
		facts cardFacts
		want  string
	}{
		{"idle running desktop", cardFacts{Status: "running"}, CardRunning},
		{"paused desktop", cardFacts{Status: "paused"}, CardPaused},
		{"stopped desktop", cardFacts{Status: "stopped"}, CardStopped},
		{"crashed desktop", cardFacts{Status: "stopped", Crash: "out of memory"}, CardCrashed},
		{"working", cardFacts{Status: "running", TurnRunning: true, Live: true, LastSignal: recent}, CardWorking},
		{"recording", cardFacts{Status: "running", TurnRunning: true, Ticket: true, Live: true, LastSignal: recent}, CardRecording},
		{"a turn proves a listed-stopped desktop up", cardFacts{Status: "stopped", TurnRunning: true, Live: true, LastSignal: recent}, CardWorking},
		{"a question waits", cardFacts{Status: "running", TurnRunning: true, Live: true, Waiting: true, LastSignal: recent}, CardNeedsYou},
		{"a finished report, idle", cardFacts{Status: "running", Reported: true}, CardNeedsYou},
		{"a finished report does not hide new work", cardFacts{Status: "running", Reported: true, TurnRunning: true, Live: true, LastSignal: recent}, CardWorking},
		{"a question on a stopped desktop is not right now", cardFacts{Status: "stopped", Waiting: true}, CardStopped},
		// The records closed the task and nothing came since: the chat's leftover turn is not work.
		{"stale turn", cardFacts{Status: "running", TurnRunning: true, Live: false, LastSignal: quiet}, CardRunning},
		{"stale turn on a crashed desktop", cardFacts{Status: "stopped", Crash: "exit 137", TurnRunning: true, LastSignal: quiet}, CardCrashed},
		// Work the desktop started on its own (no record yet) that it reports on is still shown.
		{"unrecorded but heard from", cardFacts{Status: "running", TurnRunning: true, Live: false, LastSignal: recent}, CardWorking},
		{"quiet for long", cardFacts{Status: "running", TurnRunning: true, Live: true, LastSignal: silent}, CardNoSignal},
		{"other docker status passes through", cardFacts{Status: "restarting"}, "restarting"},
	}
	for _, c := range cases {
		if got := deriveCardState(c.facts, now); got.State != c.want {
			t.Errorf("%s: state %q (%+v), want %q", c.name, got.State, got, c.want)
		}
	}
	if got := deriveCardState(cardFacts{Status: "running", TurnRunning: true, Live: true, LastSignal: silent}, now); !strings.Contains(got.Why, "15 minutes") {
		t.Errorf("no-signal says %q, want how long", got.Why)
	}
}

// TestActivityOfRecordsWhenLastHeard: a running turn carries when it was last heard from (its latest step), which
// the derived state reads for no-signal and stale turns, and the poll serves it.
func TestActivityOfRecordsWhenLastHeard(t *testing.T) {
	s := chatServer(t)
	route := deliver(t, s, "box", "file the report")
	s.Chat.AppendTaskStep("box", route.TaskID, "opened the form", nil)

	a := activityOf(s.Chat.Snapshot("box"))
	if !a.Running || a.SignalAt.IsZero() || time.Since(a.SignalAt) > time.Minute {
		t.Fatalf("activity = %+v, want a running turn heard from just now", a)
	}

	rec := httptest.NewRecorder()
	s.activityHandler(rec, httptest.NewRequest(http.MethodGet, "/api/desktops/activity?ids=box", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("activity: %d", rec.Code)
	}
}
