package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"

	"github.com/YKoustubhRao/decurix/internal/desktop"
)

// fakeDesktopManager builds a *desktop.Manager backed by a throwaway HTTP
// server standing in for the Docker daemon, reporting exactly the given
// desktop IDs as existing containers. Enough for handlers that only ever
// call Manager.Get to validate a desktop ID before doing something else
// (createScheduleHandler, linkDesktopMemoryHandler) - there is no real
// Docker daemon in a unit test.
func fakeDesktopManager(t *testing.T, existingIDs ...string) *desktop.Manager {
	t.Helper()

	exists := make(map[string]bool, len(existingIDs))
	for _, id := range existingIDs {
		exists[desktop.SanitizeID(id)] = true
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A page that lists desktops (the triggers page) gets none: only Get
		// knows the given IDs.
		if strings.HasSuffix(r.URL.Path, "/containers/json") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("[]"))

			return
		}

		for id := range exists {
			if strings.Contains(r.URL.Path, "/containers/desktop_"+id+"/json") {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(container.InspectResponse{
					ContainerJSONBase: &container.ContainerJSONBase{ID: id},
				})

				return
			}
		}

		http.NotFound(w, r)
	}))
	t.Cleanup(ts.Close)

	cli, err := client.NewClientWithOpts(
		client.WithHTTPClient(ts.Client()),
		client.WithHost("tcp://"+strings.TrimPrefix(ts.URL, "http://")),
		client.WithVersion("1.43"),
	)
	if err != nil {
		t.Fatal(err)
	}

	mgr := desktop.NewManager(cli)
	// A real control plane has its secret; without one, creation is refused (agent tokens).
	mgr.HostGUISecret = []byte("test secret")
	return mgr
}
