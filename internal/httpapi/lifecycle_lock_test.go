package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
)

// A stop, pause or delete waits for a delivery that holds the desktop's lifecycle lock, so it never lands between a
// request reaching the desktop and its record (B-23).
func TestADesktopActionWaitsForTheLifecycleLock(t *testing.T) {
	s := &Server{Desktops: &desktop.Manager{}}
	unlock := s.Desktops.LockDesktop("box")

	acted := make(chan struct{})
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/api/desktops/stop", strings.NewReader("id=box"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		s.desktopAction(httptest.NewRecorder(), req, false, func(context.Context, string) error {
			close(acted)
			return nil
		})
	}()

	select {
	case <-acted:
		t.Fatal("the action ran while a delivery held the lock")
	case <-time.After(200 * time.Millisecond):
	}
	unlock()
	select {
	case <-acted:
	case <-time.After(5 * time.Second):
		t.Fatal("the action never ran after the lock was let go")
	}
}

// The banner's status says "not running" only once Docker was asked and did not answer.
func TestDockerStatusSaysWhatWasSeen(t *testing.T) {
	s := &Server{Desktops: &desktop.Manager{}}
	rec := httptest.NewRecorder()
	s.dockerStatusHandler(rec, httptest.NewRequest(http.MethodGet, "/api/docker/status", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"checked":false`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

// A stop closes the desktop's open tasks while it still holds the lifecycle lock, so a delivery waiting on that lock
// records its task only after the close (RW1-E-01): the lock is still held when the tasks are closed.
func TestAStopClosesItsTasksUnderTheLock(t *testing.T) {
	s := chatServer(t)
	s.Desktops = &desktop.Manager{}
	deliver(t, s, "box", "a task")
	heldDuringClose := false
	act := s.thenClose(func(context.Context, string) error { return nil }, "stopped")
	unlock := s.Desktops.LockDesktop("box")
	done := make(chan struct{})
	go func() {
		// Another delivery waiting on the lock: it must not get it before the close below.
		u := s.Desktops.LockDesktop("box")
		heldDuringClose = s.Tasks.Working() == false
		u()
		close(done)
	}()
	if err := act(context.Background(), "box"); err != nil {
		t.Fatal(err)
	}
	unlock()
	<-done
	if !heldDuringClose {
		t.Fatal("the waiting delivery got the lock before the tasks were closed")
	}
}
