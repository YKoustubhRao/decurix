// Tests for requests and messages that survive a restart (deliveries.go): a send cut short between delivery and
// record is settled with its desktop at the next start, and a desktop's resent message is applied once.
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/delivery"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// deliveryServer is chatServer with the delivery records.
func deliveryServer(t *testing.T) *Server {
	t.Helper()
	s := chatServer(t)
	store, err := delivery.NewStore(testDB(t))
	if err != nil {
		t.Fatal(err)
	}
	s.Deliveries = store
	return s
}

// cutShort writes a request down as sending, as submitStored does, and then "crashes": nothing is recorded.
func cutShort(t *testing.T, s *Server, desktopID, content string) tasks.Route {
	t.Helper()
	if err := s.Tasks.UpsertWorker(tasks.Worker{ID: desktopID, Engine: "thebot"}); err != nil {
		t.Fatal(err)
	}
	route := s.routeRequest(desktopID, content, tasks.Origin{Kind: tasks.OriginPerson})
	if s.beginSending(context.Background(), desktopID, content, route) == 0 {
		t.Fatal("the send was not written down")
	}
	return route
}

// restart makes every send so far one the previous process began.
func restart(t *testing.T) {
	t.Helper()
	before := processStarted
	processStarted = time.Now().Add(time.Second)
	t.Cleanup(func() { processStarted = before })
}

func TestCutShortSendTheDesktopHadIsRecorded(t *testing.T) {
	s := deliveryServer(t)
	route := cutShort(t, s, "box", "file the report")
	restart(t)
	s.askReceivedFn = func(_ context.Context, desktopID, taskID string, _ time.Time) (bool, bool) {
		return desktopID == "box" && taskID == route.TaskID, true
	}

	s.ReconcileDeliveries(context.Background())

	detail, err := s.Tasks.Get(route.TaskID)
	if err != nil || detail.Request != "file the report" {
		t.Fatalf("task = %+v, %v; want the request recorded", detail, err)
	}
	if left, _ := s.Deliveries.Leftovers(processStarted); len(left) != 0 {
		t.Fatalf("the settled send is still there: %+v", left)
	}
}

func TestCutShortSendTheDesktopNeverGotIsDroppedAndSaid(t *testing.T) {
	s := deliveryServer(t)
	route := cutShort(t, s, "box", "file the report")
	restart(t)
	s.askReceivedFn = func(context.Context, string, string, time.Time) (bool, bool) { return false, true }

	s.ReconcileDeliveries(context.Background())

	if _, err := s.Tasks.Get(route.TaskID); err == nil {
		t.Fatal("a request the desktop never got was recorded")
	}
	if text := chatText(t, s, "box"); !strings.Contains(text, "send it again") {
		t.Fatalf("the person was not told: %q", text)
	}
}

func TestUnconfirmedSendAcceptsItsLateOutcome(t *testing.T) {
	s := deliveryServer(t)
	route := cutShort(t, s, "box", "file the report")
	restart(t)
	s.askReceivedFn = func(context.Context, string, string, time.Time) (bool, bool) { return false, false }

	s.ReconcileDeliveries(context.Background())

	if _, err := s.Tasks.Get(route.TaskID); err == nil {
		t.Fatal("an unconfirmed send was recorded before its desktop answered")
	}
	if text := chatText(t, s, "box"); !strings.Contains(text, "may not have reached") {
		t.Fatalf("the person was not told: %q", text)
	}
	if busy, _ := s.Tasks.Busy("box"); busy != "" {
		t.Fatal("an unconfirmed send made its desktop busy")
	}

	rec := postJSON(t, s.queryTextHandler, "/api/querytext",
		`{"desktop_id":"box","task_id":"`+route.TaskID+`","content":"DECURIX TASK COMPLETED","kind":"final","reply":"Filed."}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("late outcome: %d %s", rec.Code, rec.Body)
	}
	detail, err := s.Tasks.Get(route.TaskID)
	if err != nil || detail.Request != "file the report" || detail.Status != tasks.StatusDone {
		t.Fatalf("task = %+v, %v; want the request and its outcome recorded", detail, err)
	}

	// Another task id is still refused, as before.
	rec = postJSON(t, s.queryTextHandler, "/api/querytext",
		`{"desktop_id":"box","task_id":"t0123456789abcdef","content":"x","kind":"final","reply":"x"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("an outcome for a task nobody opened: %d, want 409", rec.Code)
	}
}

func TestReconcileLeavesAnAlreadyRecordedSendAlone(t *testing.T) {
	s := deliveryServer(t)
	route := cutShort(t, s, "box", "file the report")
	if _, _, err := s.Tasks.CommitReply(route, tasks.Worker{ID: "box"}, "file the report", false, tasks.ReplyBy{}, route.Origin); err != nil {
		t.Fatal(err)
	}
	restart(t)
	asked := false
	s.askReceivedFn = func(context.Context, string, string, time.Time) (bool, bool) { asked = true; return true, true }

	s.ReconcileDeliveries(context.Background())

	if asked {
		t.Fatal("a recorded send was asked about again")
	}
	if text := chatText(t, s, "box"); strings.Count(text, "file the report") > 0 {
		t.Fatalf("the recorded request was added to the chat again: %q", text)
	}
}

func TestSendingKeepsOnlyTheWithheldForm(t *testing.T) {
	s := deliveryServer(t)
	ctx := context.WithValue(context.Background(), sentSecretKey{}, true)
	route := s.routeRequest("box", "the code is 482913", tasks.Origin{Kind: tasks.OriginPerson})
	s.beginSending(ctx, "box", "the code is 482913", route)
	left, _ := s.Deliveries.Leftovers(time.Now().Add(time.Second))
	if len(left) != 1 || strings.Contains(string(left[0].Payload), "482913") {
		t.Fatalf("the secret was written down as typed: %s", left[0].Payload)
	}
}

func TestResentMessageIsAppliedOnce(t *testing.T) {
	s := deliveryServer(t)
	route := deliver(t, s, "box", "file the report")
	question := `{"desktop_id":"box","task_id":"` + route.TaskID + `","content":"DECURIX QUERY REQUEST","kind":"query",
		"reply":"Which year?","event_id":"0123456789abcdef0123456789abcdef"}`

	first := postJSON(t, s.queryTextHandler, "/api/querytext", question)
	second := postJSON(t, s.queryTextHandler, "/api/querytext", question)
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("codes %d, %d", first.Code, second.Code)
	}
	var answer map[string]any
	_ = json.Unmarshal(second.Body.Bytes(), &answer)
	if answer["duplicate"] != true {
		t.Fatalf("the resend was not acknowledged as a duplicate: %s", second.Body)
	}
	if n := strings.Count(chatText(t, s, "box"), "Which year?"); n != 1 {
		t.Fatalf("the question is in the chat %d times, want once", n)
	}
	if notes := s.QueryNotes.List(); len(notes) != 1 {
		t.Fatalf("%d Queries notes, want 1", len(notes))
	}
}

// chatText is everything a desktop's chat shows, joined.
func chatText(t *testing.T, s *Server, desktopID string) string {
	t.Helper()
	var parts []string
	for _, entry := range readThread(t, s, desktopID).Entries {
		parts = append(parts, entry.Content)
	}
	return strings.Join(parts, "\n")
}

// TestABootingDesktopIsAskedAgain: the desktop the cut-short send had just started answers only once its agent is up;
// the check asks again rather than settling for "cannot say" (found live 2026-11-11).
func TestABootingDesktopIsAskedAgain(t *testing.T) {
	s := deliveryServer(t)
	route := cutShort(t, s, "box", "file the report")
	restart(t)
	before := reconcileRetry
	reconcileRetry = 10 * time.Millisecond
	t.Cleanup(func() { reconcileRetry = before })
	asks := 0
	s.askReceivedFn = func(context.Context, string, string, time.Time) (bool, bool) {
		asks++
		return false, asks >= 3 // up on the third ask, and it never got the request
	}
	s.bootingFn = func(string) bool { return true }

	s.ReconcileDeliveries(context.Background())

	if asks != 3 {
		t.Fatalf("asked %d times, want 3", asks)
	}
	if text := chatText(t, s, "box"); !strings.Contains(text, "send it again") {
		t.Fatalf("the person was not told to send it again: %q", text)
	}
	if _, err := s.Tasks.Get(route.TaskID); err == nil {
		t.Fatal("a request the desktop never got was recorded")
	}
}
