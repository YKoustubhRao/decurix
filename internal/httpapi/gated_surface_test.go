package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// hostTouchingRoutes read or act on the host - its files, its apps and sign-ins, the owner's accounts - or route
// work by guessing. The gated listener (the one the tunnel and the LAN reach) must serve none of them, whether they
// are registered in RegisterLocalRoutes (the walk in TestNoPrimaryOnlyRouteIsOnTheGatedListener checks those) or
// were deleted (review 2026-11-03, X-4). Named here so that re-adding one to RegisterRoutes fails a test.
var hostTouchingRoutes = []string{
	"/api/host-paths/validate", // deleted: an unscoped stat of any host path (A-01)
	"/api/host-paths/browse",
	"/api/host-apps/detect",        // the host's apps and where their sign-ins are kept (A-02)
	"/api/desktops/host-apps/copy", // the host's programs into a desktop (N-B2)
	"/api/claude-auth/status",      // deleted: the owner's Claude account (A-04); no page signed in through it any more
	"/api/claude-auth/login/start",
	"/api/claude-auth/login/code",
	"/api/claude-auth/login/cancel",
	"/api/channels/inbound", // deleted: routed outside text to any running desktop by keywords (B-06)
	"/api/host-bridge/exec",
	"/api/host-bridge/open",
	"/api/host-files/stat",
	"/api/host-files/read",
	"/api/backup",
	"/api/desktops/restore",
	"/api/dags/runs/cancel",
}

func TestTheGatedListenerServesNoHostSurface(t *testing.T) {
	_, gated := listenerMuxes(fullServer(t))
	for _, path := range hostTouchingRoutes {
		// Not itself, and not through a subtree or a mounted sub-mux either: only "no route" or the catch-all page
		// may answer it (RW1-F-07).
		if _, pattern := gated.Handler(httptest.NewRequest(http.MethodGet, path, nil)); pattern != "" && pattern != "/" {
			t.Errorf("%s is served on the gated listener (as %q)", path, pattern)
		}
	}
}

// A desktop made through the phone or tunnel address gets none of this computer's apps (W-05).
func TestAGatedCreateIsRefusedHostApps(t *testing.T) {
	srv := &Server{Desktops: fakeDesktopManager(t)}
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	gated := MarkGated(mux)

	for _, form := range []url.Values{
		{"name": {"from-phone"}, "copy_host_apps": {"1"}, "host_apps": {"some-app"}},
		{"name": {"from-phone"}, "host_apps": {"some-app"}},
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/desktops/create", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		gated.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "this computer") {
			t.Errorf("%v: %d %s, want 403", form, rec.Code, rec.Body)
		}
	}
}

// The organisation's skills and knowledge are read from the gated listener, never changed from it (O-7).
func TestOrgWritesAreRefusedFromTheGatedListener(t *testing.T) {
	s := fullServer(t)
	called := false
	h := MarkGated(s.readsForAll(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/knowledge", strings.NewReader("{}")))
	if rec.Code != http.StatusForbidden || called {
		t.Fatalf("gated write: %d, handler called %v", rec.Code, called)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/knowledge", nil))
	if !called {
		t.Fatalf("gated read refused: %d", rec.Code)
	}
}

// Host-state writes the gated listener may read but never make: the Mobile menu's links (RW1-F-01).
func TestTheGatedListenerCannotRewriteTheLinks(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tunnel", strings.NewReader(`{"url":"https://elsewhere.example"}`))
	req.Header.Set("Content-Type", "application/json")
	MarkGated(http.HandlerFunc(s.tunnelHandler)).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("gated POST /api/tunnel: %d %s", rec.Code, rec.Body)
	}
}
