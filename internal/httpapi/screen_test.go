package httpapi

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"

	"github.com/YKoustubhRao/decurix/internal/conversation"
	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/tasks"
	"github.com/YKoustubhRao/decurix/internal/webui"
)

// screenDesktop is a Docker API with one stopped desktop, "terga", whose
// container carries the screen password and the name a person gave it.
func screenDesktop(t *testing.T) *desktop.Manager {
	t.Helper()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/containers/desktop_terga/json") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(container.InspectResponse{
			ContainerJSONBase: &container.ContainerJSONBase{ID: "terga", State: &container.State{Status: "exited"}},
			Config:            &container.Config{Env: []string{"VNC_PW=s3cret", "DESKTOP_NAME=Ops desk", "DESKTOP_ID=terga"}},
		})
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
	return desktop.NewManager(cli)
}

func screenMux(t *testing.T) *http.ServeMux {
	t.Helper()
	srv := &Server{Desktops: screenDesktop(t), Web: webui.NewRenderer()}
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	return mux
}

// The screen is the control plane's own page, served even for a stopped
// desktop (so it can offer to start it), with the password in its script and
// never cached.
func TestScreenIsTheControlPlanesViewer(t *testing.T) {
	mux := screenMux(t)

	for _, path := range []string{"/desktop/terga/", "/desktop/terga", "/desktop/terga/?autoconnect=true&resize=remote&ui=touch"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d %s", path, rec.Code, rec.Body)
		}
		page := rec.Body.String()
		for _, want := range []string{`<title>Ops desk</title>`, `const PASSWORD = "s3cret"`, `const ID = "terga"`,
			`id="decurix-ticket"`, `var DESKTOP_ID = "terga"`, noVNCPath} {
			if !strings.Contains(page, want) {
				t.Fatalf("%s: the viewer is missing %q", path, want)
			}
		}
		if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("%s: a page holding a password must not be cached or leak a referrer: %v", path, rec.Header())
		}
		if strings.Contains(page, "noVNC_password") || strings.Contains(page, "__DESKTOP_ID__") {
			t.Fatalf("%s: the old noVNC page's pieces leaked into the viewer", path)
		}
	}
}

func TestScreenOfADesktopThatDoesNotExistIsNotFound(t *testing.T) {
	rec := httptest.NewRecorder()
	screenMux(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/desktop/nope/", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d", rec.Code)
	}
}

// Everything under the screen is still the desktop's: its WebSocket and
// noVNC's own page, for whoever wants it, go through the proxy.
func TestNoVNCsOwnPageIsStillProxied(t *testing.T) {
	rec := httptest.NewRecorder()
	screenMux(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/desktop/terga/vnc.html", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "not running") {
		t.Fatalf("vnc.html of a stopped desktop: status %d %s", rec.Code, rec.Body)
	}

	req := httptest.NewRequest(http.MethodGet, "/desktop/terga/", nil)
	req.Header.Set("Upgrade", "websocket")
	rec = httptest.NewRecorder()
	screenMux(t).ServeHTTP(rec, req)
	if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "<!doctype html>") {
		t.Fatal("a WebSocket upgrade was answered with the viewer page")
	}
}

// The RFB client is one file, cached for good, gzipped for browsers that take it.
func TestNoVNCBundleIsServedOnceAndCachedForGood(t *testing.T) {
	mux := screenMux(t)

	req := httptest.NewRequest(http.MethodGet, noVNCPath, nil)
	req.Header.Set("Accept-Encoding", "gzip, br")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "gzip" ||
		!strings.Contains(rec.Header().Get("Cache-Control"), "immutable") ||
		!strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") {
		t.Fatalf("status %d headers %v", rec.Code, rec.Header())
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(zr)
	if !bytes.Equal(plain, webui.NoVNC) || !bytes.Contains(plain, []byte("export{")) {
		t.Fatal("the gzipped bundle is not the embedded module")
	}
	if rec.Body.Len() > len(webui.NoVNC)/2 {
		t.Fatalf("gzip saved too little: %d of %d", rec.Body.Len(), len(webui.NoVNC))
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, noVNCPath, nil))
	if rec.Header().Get("Content-Encoding") != "" || !bytes.Equal(rec.Body.Bytes(), webui.NoVNC) {
		t.Fatal("a browser that takes no gzip got something else")
	}
}

// An open screen keeps its desktop from idle-pausing for as long as it is
// open, and stops the moment it closes.
func TestAnOpenScreenKeepsItsDesktopViewed(t *testing.T) {
	saved := screenViewedEvery
	screenViewedEvery = 5 * time.Millisecond
	t.Cleanup(func() { screenViewedEvery = saved })

	mgr := screenDesktop(t)
	srv := &Server{Desktops: mgr}

	if _, ok := mgr.LastViewed("terga"); ok {
		t.Fatal("viewed before anything opened it")
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		srv.keepScreenViewed("terga", done)
		close(finished)
	}()

	waitFor(t, 2*time.Second, "an open screen to mark its desktop viewed", func() bool {
		_, ok := mgr.LastViewed("terga")
		return ok
	})

	close(done)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("closing the screen did not stop the keepalive")
	}
}

// A restarted desktop reports the task it lost; recorded only while that task
// is still open, so a restart through the API (which closed it and said so)
// does not get a second note.
func TestAnInterruptedTaskIsReportedOnlyWhileOpen(t *testing.T) {
	s := chatServer(t)

	route := deliver(t, s, "terga", "/learn demo-20260923T211809-b176b6")
	interrupted := `{"desktop_id":"terga","task_id":"` + route.TaskID + `","content":"DECURIX TASK COMPLETED","kind":"error",
	  "reply":"This desktop restarted while it was working on this.","interrupted":true}`

	postJSON(t, s.queryTextHandler, "/api/querytext", interrupted)

	detail, err := s.Tasks.Get(route.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Status != tasks.StatusError {
		t.Fatalf("an open task interrupted by a restart should end as an error, got %s", detail.Status)
	}
	thread := readThread(t, s, "terga")
	if thread.Running || thread.Entries[len(thread.Entries)-1].Kind != conversation.KindError {
		t.Fatalf("the chat should show the restart and stop working: %+v", thread)
	}

	// Already closed: nothing more.
	second := deliver(t, s, "terga", "hello")
	if err := s.Tasks.CloseOpen("terga", "The desktop was restarted before this task finished."); err != nil {
		t.Fatal(err)
	}
	entries := len(readThread(t, s, "terga").Entries)
	notes := len(s.QueryNotes.List())

	rec := postJSON(t, s.queryTextHandler, "/api/querytext", strings.Replace(interrupted, route.TaskID, second.TaskID, 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if len(readThread(t, s, "terga").Entries) != entries || len(s.QueryNotes.List()) != notes {
		t.Fatal("a task the control plane had already closed got a second note")
	}
}
