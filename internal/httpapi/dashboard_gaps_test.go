package httpapi

// Route tests for the dashboard's dead ends closed on 2026-11-07: the home folder an earlier desktop left (the create
// form's 409 and the person's delete of it), worker memory listed with a limit, and a held run's recording deleted
// only with force.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/volume"

	"github.com/YKoustubhRao/decurix/internal/memory"
)

// leftoverDaemon stands in for Docker with desktop "box" gone and its home volume left, labelled by an earlier
// creation. It notes every volume removal it is asked for.
type leftoverDaemon struct {
	mu      sync.Mutex
	removed []string
}

func (d *leftoverDaemon) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/containers/json"):
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	case strings.HasSuffix(path, "/volumes/desktop_box_home") && r.Method == http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(volume.Volume{Name: "desktop_box_home", Labels: map[string]string{
			"decurix": "true", "decurix.desktop": "box", "decurix.creation": "earlier"}})
	case strings.HasSuffix(path, "/volumes/desktop_box_home") && r.Method == http.MethodDelete:
		d.mu.Lock()
		d.removed = append(d.removed, path+"?"+r.URL.RawQuery)
		d.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func leftoverServer(t *testing.T) (*Server, *leftoverDaemon) {
	t.Helper()
	d := &leftoverDaemon{}
	return &Server{Desktops: managerOn(t, httptest.NewServer(http.HandlerFunc(d.serve)))}, d
}

// sendForm posts form to handler from this computer's own browser (loopback), through the gated mark when gated.
func sendForm(handler http.Handler, path string, form url.Values, gated bool) *httptest.ResponseRecorder {
	if gated {
		handler = MarkGated(handler)
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "127.0.0.1:50000"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestCreateSaysAnEarlierDesktopsHomeIsLeftBeforeStarting(t *testing.T) {
	s, d := leftoverServer(t)
	create := http.HandlerFunc(s.createDesktopHandler)

	rec := sendForm(create, "/api/desktops/create", url.Values{"name": {"Box"}}, false)
	var body struct {
		Error        string `json:"error"`
		LeftoverHome bool   `json:"leftover_home"`
		ID           string `json:"id"`
	}
	if rec.Code != http.StatusConflict || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
		t.Fatalf("create: %d %s, want a 409", rec.Code, rec.Body)
	}
	if !body.LeftoverHome || body.ID != "box" || !strings.Contains(body.Error, "earlier desktop called box") {
		t.Fatalf("create's answer = %+v", body)
	}
	if _, ok := s.Desktops.GetProvisioning("box"); ok {
		t.Fatal("a creation was started for a name whose old home is left")
	}

	// Through the phone or tunnel address the delete is not served, so it is not offered there.
	body.LeftoverHome = true
	rec = sendForm(create, "/api/desktops/create", url.Values{"name": {"box"}}, true)
	if rec.Code != http.StatusConflict || json.Unmarshal(rec.Body.Bytes(), &body) != nil || body.LeftoverHome {
		t.Fatalf("gated create: %d %s, want a 409 offering no delete", rec.Code, rec.Body)
	}
	if len(d.removed) > 0 {
		t.Fatalf("a create removed %v", d.removed)
	}
}

func TestTheLeftoverHomeIsDeletedOnlyAsAPersonAsks(t *testing.T) {
	s, d := leftoverServer(t)
	primary := http.NewServeMux()
	s.RegisterLocalRoutes(primary)

	rec := sendForm(primary, "/api/desktops/home-volume/delete", url.Values{"name": {"box"}}, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if len(d.removed) != 1 || strings.Contains(d.removed[0], "force=1") {
		t.Fatalf("removed %v, want the home volume once, never forced", d.removed)
	}

	// Nothing of another name, and a name that is no desktop's, is touched.
	for _, name := range []string{"other", "../etc", ""} {
		rec = sendForm(primary, "/api/desktops/home-volume/delete", url.Values{"name": {name}}, false)
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusBadRequest {
			t.Errorf("%q: %d %s, want 404 or 400", name, rec.Code, rec.Body)
		}
	}
	if len(d.removed) != 1 {
		t.Fatalf("removed %v", d.removed)
	}
}

func TestTheLeftoverHomeDeleteIsForPeopleOnTheirOwnComputer(t *testing.T) {
	_, gated := listenerMuxes(fullServer(t))
	path := "/api/desktops/home-volume/delete"
	if _, pattern := gated.Handler(httptest.NewRequest(http.MethodPost, path, nil)); pattern != "" && pattern != "/" {
		t.Fatalf("%s is served on the gated listener (as %q)", path, pattern)
	}

	// A desktop's agent (an address that is not this computer's) is refused before anything is looked up.
	s, d := leftoverServer(t)
	s.netsFor = staticNets("172.30.0.0/16", "172.30.0.1")
	mux := http.NewServeMux()
	s.RegisterLocalRoutes(mux)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("name=box"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "172.30.0.5:40000"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || len(d.removed) > 0 {
		t.Fatalf("from a desktop: %d %s, removed %v", rec.Code, rec.Body, d.removed)
	}
}

func TestWorkerMemoryIsListedNewestFirstWithALimit(t *testing.T) {
	store, err := memory.NewStore(testDB(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, title := range []string{"first", "second", "third"} {
		if _, err := store.Record("box", title, "a lesson", "workflow"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	s := &Server{Memory: store}

	list := func(query string) (got struct {
		Entries   []memory.WorkerMemoryEntry `json:"entries"`
		Limit     int                        `json:"limit"`
		Truncated bool                       `json:"truncated"`
	}) {
		t.Helper()
		rec := httptest.NewRecorder()
		s.desktopMemoryHandler(rec, httptest.NewRequest(http.MethodGet, "/api/desktops/memory"+query, nil))
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
			t.Fatalf("GET %s: %d %s", query, rec.Code, rec.Body)
		}
		return got
	}

	cut := list("?limit=2")
	if len(cut.Entries) != 2 || !cut.Truncated || cut.Limit != 2 || cut.Entries[0].Title != "third" {
		t.Fatalf("limit=2: %+v", cut)
	}
	if all := list(""); len(all.Entries) != 3 || all.Truncated || all.Limit != memoryListDefault {
		t.Fatalf("default: %+v", all)
	}
	for query, want := range map[string]int{"?limit=0": memoryListDefault, "?limit=x": memoryListDefault, "?limit=99999": memoryListMax} {
		if got := list(query).Limit; got != want {
			t.Errorf("%s: limit %d, want %d", query, got, want)
		}
	}
}

func TestAHeldRunsRecordingIsDeletedOnlyWithForce(t *testing.T) {
	s := chatServer(t)
	var bodies []string
	s.agentRequest = func(_ context.Context, id, method, path string, body io.Reader) (*http.Response, error) {
		raw, _ := io.ReadAll(body)
		bodies = append(bodies, string(raw))
		if path != "/recordings/task-t1/delete" || method != http.MethodPost {
			t.Fatalf("called %s %s", method, path)
		}
		answer := func(code int, v any) *http.Response {
			b, _ := json.Marshal(v)
			return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(bytes.NewReader(b))}
		}
		if !strings.Contains(string(raw), `"force":true`) {
			return answer(http.StatusConflict, map[string]any{"held": true, "error": "This run is not saved to the run record yet; delete it anyway?"}), nil
		}
		return answer(http.StatusOK, map[string]string{"status": "deleted"}), nil
	}

	rec := postForm(t, s.recordingDeleteHandler, "/x", "id=box&session=task-t1")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"held":true`) {
		t.Fatalf("without force: %d %s, want the desktop's 409 with held", rec.Code, rec.Body)
	}
	rec = postForm(t, s.recordingDeleteHandler, "/x", "id=box&session=task-t1&force=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("with force: %d %s", rec.Code, rec.Body)
	}
	if len(bodies) != 2 || strings.Contains(bodies[0], "force") || !strings.Contains(bodies[1], `"force":true`) {
		t.Fatalf("bodies sent = %q", bodies)
	}
}
