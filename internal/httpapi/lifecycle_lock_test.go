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
