package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/memory"
	"github.com/YKoustubhRao/decurix/internal/tunnel"
	"github.com/YKoustubhRao/decurix/internal/webui"
	"github.com/docker/docker/client"
)

// With Docker down (a client of a daemon that is not there, so listing errs), the dashboard, Requests, Triggers and DAG pages
// still render, with no desktops, so their Docker banner can say why (found by the 2026-11-07 smoke test: they answered
// "internal server error" and the banner never showed).
func TestPagesRenderWhenDockerIsDown(t *testing.T) {
	s := fullServer(t)
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://127.0.0.1:1"), client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	s.Desktops = desktop.NewManager(cli)
	if s.Memory, err = memory.NewStore(testDB(t)); err != nil {
		t.Fatal(err)
	}
	s.Web = webui.NewRenderer()
	s.Tunnel = tunnel.NewStore()
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)
	s.RegisterLocalRoutes(mux)
	for _, page := range []string{"/", "/requests", "/triggers"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, page, nil)
		req.RemoteAddr = "127.0.0.1:50000" // the person at this computer, not a desktop
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s with Docker down: %d %.200s", page, rec.Code, rec.Body.String())
			continue
		}
		if !strings.Contains(rec.Body.String(), `id="dockerBanner"`) {
			t.Errorf("%s has no Docker banner to say why it lists no desktops", page)
		}
	}
}
