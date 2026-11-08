package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/memory"
	"github.com/YKoustubhRao/decurix/internal/querynotes"
	"github.com/YKoustubhRao/decurix/internal/resourcemon"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// overviewServer is a chat server whose stand-in Docker daemon lists the stopped desktops ids, counting each list it
// is asked for, with memory and a resource monitor.
func overviewServer(t *testing.T, ids ...string) (*Server, *atomic.Int64) {
	t.Helper()
	s := chatServer(t)
	var err error
	if s.Memory, err = memory.NewStore(testDB(t)); err != nil {
		t.Fatal(err)
	}

	var lists atomic.Int64
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/containers/json") {
			http.NotFound(w, r)
			return
		}
		lists.Add(1)
		var out []container.Summary
		for _, id := range ids {
			// Labelled as a current desktop is, so the list needs no inspect.
			out = append(out, container.Summary{ID: "c-" + id, Names: []string{"/desktop_" + id}, State: "exited",
				Status: "Exited (0) 1 minute ago", Created: 1_700_000_000,
				Labels: map[string]string{"desktop-manager": "true", "desktop-model": "m", "desktop-cpus": "2", "desktop-memory": "2048"}})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	s.Desktops = managerOn(t, daemon)

	var listed []desktop.Desktop
	for _, id := range ids {
		listed = append(listed, desktop.Desktop{ID: id, Name: id, Status: "stopped", CPUs: 2, MemoryMB: 2048})
	}
	s.ResourceMon = resourcemon.NewMonitor(&mockResourceDesktopManager{desktops: listed})
	return s, &lists
}

func getOverview(t *testing.T, s *Server, query string) Overview {
	t.Helper()
	rec := httptest.NewRecorder()
	s.overviewHandler(rec, httptest.NewRequest(http.MethodGet, "/api/desktops/overview"+query, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("overview%s: %d %s", query, rec.Code, rec.Body)
	}
	var out Overview
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// sameJSON fails unless a and b encode the same.
func sameJSON(t *testing.T, what string, a, b any) {
	t.Helper()
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if !bytes.Equal(ja, jb) {
		t.Errorf("%s:\n overview %s\n own poll %s", what, ja, jb)
	}
}

// One request answers what the card's own polls answered, for every desktop, with one Docker list however many cards
// there are (W-14).
func TestOverviewAnswersEveryCardWithOneDockerList(t *testing.T) {
	s, lists := overviewServer(t, "a", "b", "c")

	s.Chat.AppendUserRequest("a", "t0", "count the invoices")
	box := tasks.Worker{ID: "a", Name: "a"}
	if _, err := s.Tasks.Commit(tasks.NewRoute(), box, "count the invoices", tasks.Origin{Kind: tasks.OriginPerson}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Memory.Record("a", "Invoices", "They live in ~/work/invoices", "files"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Memory.Record("elsewhere", "Not asked for", "Another worker's", "files"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResourceMon.RecordReport("b", 97, 97); err != nil {
		t.Fatal(err)
	}
	s.QueryNotes.Add(querynotes.QueryNote{ID: "n1", DesktopID: "c", Kind: querynotes.KindQuery, CreatedAt: time.Now(), Content: "Which file?"})

	got := getOverview(t, s, "")
	if n := lists.Load(); n != 1 {
		t.Fatalf("Docker lists for one overview of three cards = %d, want 1", n)
	}
	if len(got.Desktops) != 3 {
		t.Fatalf("cards = %v, want a, b and c", got.Desktops)
	}
	a := got.Desktops["a"]
	if a.Activity == nil || !a.Activity.Running || a.Activity.Request != "count the invoices" || a.Activity.Status != "stopped" || a.Activity.CPUs != 2 {
		t.Errorf("a's activity = %+v", a.Activity)
	}
	if a.Tasks == nil || len(*a.Tasks) != 1 || (*a.Tasks)[0].Request != "count the invoices" {
		t.Errorf("a's tasks = %+v", a.Tasks)
	}
	if a.Memory == nil || len(*a.Memory) != 1 || (*a.Memory)[0].Title != "Invoices" {
		t.Errorf("a's memory = %+v", a.Memory)
	}
	if a.Alert != nil || got.Desktops["b"].Alert == nil || got.Desktops["b"].Alert.PredictedCPUs <= 2 {
		t.Errorf("alerts: a %+v, b %+v", a.Alert, got.Desktops["b"].Alert)
	}
	if c := got.Desktops["c"]; c.Tasks == nil || len(*c.Tasks) != 0 || c.Memory == nil || len(*c.Memory) != 0 {
		t.Errorf("c's empty parts = %+v", c)
	}
	if got.Notes == nil || len(*got.Notes) != 1 || (*got.Notes)[0].ID != "n1" {
		t.Errorf("notes = %+v", got.Notes)
	}

	// Each part is what the card's own poll says.
	sameJSON(t, "activity", map[string]*Activity{"a": a.Activity, "b": got.Desktops["b"].Activity, "c": got.Desktops["c"].Activity},
		activity(t, s, "a,b,c"))
	rec := httptest.NewRecorder()
	s.desktopTasksHandler(rec, httptest.NewRequest(http.MethodGet, "/api/desktops/tasks?ids=a", nil))
	var own map[string][]TaskPick
	_ = json.Unmarshal(rec.Body.Bytes(), &own)
	sameJSON(t, "tasks", *a.Tasks, own["a"])
	rec = httptest.NewRecorder()
	s.queryNotesHandler(rec, httptest.NewRequest(http.MethodGet, "/api/querynotes", nil))
	var notes []queryNoteView
	_ = json.Unmarshal(rec.Body.Bytes(), &notes)
	sameJSON(t, "notes", *got.Notes, notes)
}

// ?ids= answers exactly the cards named (one a page shows that is not listed included, as the activity poll does),
// and a notes-only ask (a hidden tab's) reads no desktop at all.
func TestOverviewFollowsIDsAndParts(t *testing.T) {
	s, lists := overviewServer(t, "a", "b")

	got := getOverview(t, s, "?ids=b,gone,b,xxx")
	if len(got.Desktops) != 3 || got.Desktops["a"].Activity != nil {
		t.Fatalf("cards = %+v, want b, gone and xxx", got.Desktops)
	}
	if b := got.Desktops["b"]; b.Activity == nil || b.Activity.Status != "stopped" {
		t.Errorf("b = %+v", b.Activity)
	}
	if gone := got.Desktops["gone"]; gone.Activity == nil || gone.Activity.Status != "" || gone.Tasks == nil || gone.Memory == nil {
		t.Errorf("a card the list does not know keeps its own status: %+v", gone)
	}

	// Past the list's cache, so any list would reach the daemon.
	time.Sleep(700 * time.Millisecond)
	before := lists.Load()
	hidden := getOverview(t, s, "?parts=notes")
	if len(hidden.Desktops) != 0 || hidden.Notes == nil || lists.Load() != before {
		t.Errorf("notes only: %+v, %d lists", hidden, lists.Load()-before)
	}

	tasksOnly := getOverview(t, s, "?ids=a&parts=tasks")
	if a := tasksOnly.Desktops["a"]; a.Tasks == nil || a.Activity != nil || a.Memory != nil || tasksOnly.Notes != nil || lists.Load() != before {
		t.Errorf("tasks only: %+v, %d lists", tasksOnly, lists.Load()-before)
	}

	var many []string
	for i := 0; i < maxOverviewIDs+20; i++ {
		many = append(many, fmt.Sprintf("d%03d", i))
	}
	if got := getOverview(t, s, "?parts=activity&ids="+strings.Join(many, ",")); len(got.Desktops) != maxOverviewIDs {
		t.Errorf("an overview of %d ids answered %d cards, want at most %d", len(many), len(got.Desktops), maxOverviewIDs)
	}
}

// A desktop the control plane stopped to save memory carries why to its card; one a person stopped carries nothing.
func TestOverviewCarriesARestReason(t *testing.T) {
	s, _ := overviewServer(t, "a", "b")
	s.Desktops.SetRest("a", desktop.Rest{Reason: desktop.RestIdle, Note: "Stopped after 30 minutes paused, to save memory"})

	got := getOverview(t, s, "?ids=a,b")
	if a := got.Desktops["a"]; a.RestReason != desktop.RestIdle || a.RestNote == "" {
		t.Errorf("a = %+v", a)
	}
	if b := got.Desktops["b"]; b.RestReason != "" || b.RestNote != "" {
		t.Errorf("b = %+v", b)
	}
}

// The overview is served where the polls it stands for are (both listeners), and no further: the gated listener has
// every one of them already, so it learns nothing new there.
func TestOverviewIsOnBothListenersLikeThePollsItReplaces(t *testing.T) {
	primary, gated := listenerMuxes(fullServer(t))
	for _, path := range []string{"/api/desktops/overview", "/api/desktops/activity", "/api/desktops/tasks",
		"/api/desktops/memory", "/api/desktops/resource-alerts", "/api/querynotes"} {
		for name, mux := range map[string]*http.ServeMux{"primary": primary, "gated": gated} {
			if _, pattern := mux.Handler(httptest.NewRequest(http.MethodGet, path, nil)); pattern != path {
				t.Errorf("%s on the %s listener: served as %q", path, name, pattern)
			}
		}
	}
}

// A desktop is refused the overview, as it is the card polls it reads (only its own memory is a desktop's to read,
// through its own route), on the primary listener and the gated one.
func TestOverviewIsRefusedToADesktopLikeTheCardPolls(t *testing.T) {
	s, _ := guardedServer(t)
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)
	primary := s.GuardPrimary(mux)
	gated := s.GuardGated(MarkGated(mux))
	const fromDesktop = "172.30.0.5:40000"
	for _, path := range []string{"/api/desktops/overview", "/api/desktops/overview?ids=box-a&parts=memory",
		"/api/desktops/activity?ids=box-a", "/api/desktops/tasks?ids=box-a", "/api/querynotes", "/api/desktops/resource-alerts"} {
		for name, h := range map[string]http.Handler{"primary": primary, "gated": gated} {
			if got := guardDo(h, http.MethodGet, path, fromDesktop, map[string]string{"Host": "127.0.0.1:8080"}); got.Code != http.StatusForbidden {
				t.Errorf("%s from a desktop on the %s listener: %d %s", path, name, got.Code, got.Body)
			}
		}
	}
	if !containerMayCall(http.MethodGet, "/api/desktops/memory") || containerMayCall(http.MethodGet, "/api/desktops/overview") {
		t.Error("a desktop's callbacks changed: the overview must stay off them")
	}
}
