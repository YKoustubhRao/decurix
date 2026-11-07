// Tests that /api/docker/status carries the tools-not-ready state the banner reads (desktop.Manager.ToolsNotReady;
// its classes are tested in internal/desktop).
package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/desktop"
)

func TestDockerStatusSaysWhetherTheToolsAreReady(t *testing.T) {
	s := &Server{Desktops: &desktop.Manager{}}
	rec := httptest.NewRecorder()
	s.dockerStatusHandler(rec, httptest.NewRequest(http.MethodGet, "/api/docker/status", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `"tools_not_ready":false`) || !strings.Contains(body, `"tools_not_ready_why":""`) {
		t.Fatalf("%d %s", rec.Code, body)
	}
	// With no desktop manager at all, the status still answers, ready or not unknown.
	rec = httptest.NewRecorder()
	(&Server{}).dockerStatusHandler(rec, httptest.NewRequest(http.MethodGet, "/api/docker/status", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"tools_not_ready":false`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}
