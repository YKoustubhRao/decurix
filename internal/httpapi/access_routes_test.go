package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/access"
	"github.com/YKoustubhRao/decurix/internal/querynotes"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

func accessServer(t *testing.T) (*Server, *access.Store) {
	t.Helper()
	access.PasswordIterations = 1000
	s := chatServer(t)
	store, err := access.NewStore(testDB(t))
	if err != nil {
		t.Fatal(err)
	}
	s.SetAccess(store)
	t.Cleanup(func() { s.SetAccess(nil) })
	return s, store
}

func TestGateApprovalReply(t *testing.T) {
	s, store := accessServer(t)
	if _, err := store.SavePolicy(access.Policy{ActionMatch: `\bpay\b`, ApproverRoles: []string{"finance"}}); err != nil {
		t.Fatal(err)
	}
	fin, _ := store.CreateUser("fin", "password-1", []string{"finance"})
	clerk, _ := store.CreateUser("clerk", "password-1", nil)

	reply := tasks.Route{Kind: tasks.RouteReply, TaskID: "t1"}
	s.QueryNotes.Add(querynotes.QueryNote{ID: "n1", DesktopID: "d1", Kind: querynotes.KindApproval, CreatedAt: time.Now(),
		Content: "Approval needed before I click Pay: pays $900. Nothing was clicked or sent."})

	as := func(u access.User) context.Context { return access.WithActor(context.Background(), u) }

	if err := s.gateApprovalReply(as(clerk), "d1", reply, "yes"); err == nil {
		t.Fatal("clerk's yes delivered")
	} else if refused, ok := err.(*ApprovalRefusedError); !ok || refused.Status != http.StatusForbidden || !strings.Contains(refused.Message, "finance approver") {
		t.Fatalf("refusal: %v", err)
	}
	if err := s.gateApprovalReply(context.Background(), "d1", reply, "yes"); err == nil {
		t.Fatal("a reply with no actor (an automated door) was delivered")
	}
	if err := s.gateApprovalReply(as(fin), "d1", reply, "yes"); err != nil {
		t.Fatalf("approver refused: %v", err)
	}
	if err := s.gateApprovalReply(as(access.LocalOwner), "d1", reply, "yes"); err != nil {
		t.Fatalf("local owner refused: %v", err)
	}
	if err := s.gateApprovalReply(as(clerk), "d1", tasks.Route{Kind: tasks.RouteNew}, "pay it"); err != nil {
		t.Fatalf("a new request is not a reply: %v", err)
	}

	// Closing the note by hand does not open a way around the gate.
	s.QueryNotes.Close("n1")
	if err := s.gateApprovalReply(as(clerk), "d1", reply, "yes"); err == nil {
		t.Fatal("closing the note bypassed the gate")
	}

	// An answered approval followed by a plain question is not gated.
	s.QueryNotes.ResolveAnswered("d1")
	s.QueryNotes.Add(querynotes.QueryNote{ID: "n2", DesktopID: "d1", Kind: querynotes.KindQuery, CreatedAt: time.Now().Add(time.Second), Content: "Which file?"})
	if err := s.gateApprovalReply(as(clerk), "d1", reply, "the second"); err != nil {
		t.Fatalf("question gated: %v", err)
	}

	log, _ := store.Approvals("d1", 0)
	if len(log) != 5 { // clerk, nobody, fin, owner, clerk after the close; the question is not logged
		t.Fatalf("log = %d entries, want 5", len(log))
	}

	rec := httptest.NewRecorder()
	if !writeApprovalRefused(rec, &ApprovalRefusedError{Status: http.StatusConflict, Message: "held"}) || rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), `"status":"held"`) {
		t.Fatalf("writeApprovalRefused: %d %s", rec.Code, rec.Body)
	}
}

func TestGateWithoutAccessIsOpen(t *testing.T) {
	s := chatServer(t)
	s.QueryNotes.Add(querynotes.QueryNote{ID: "n1", DesktopID: "d1", Kind: querynotes.KindApproval, CreatedAt: time.Now(), Content: "pay"})
	if err := s.gateApprovalReply(context.Background(), "d1", tasks.Route{Kind: tasks.RouteReply}, "yes"); err != nil {
		t.Fatalf("no access store wired should change nothing: %v", err)
	}
}

func TestActorContextAndRoutes(t *testing.T) {
	s, store := accessServer(t)
	_, _ = store.CreateUser("asha", "password-1", []string{"finance"})

	primary := http.NewServeMux()
	s.RegisterAccessRoutes(primary)
	s.RegisterAccessLocalRoutes(primary)
	gated := http.NewServeMux()
	s.RegisterAccessRoutes(gated)
	gatedHandler := MarkGated(gated)

	// Primary: the actor is the local owner.
	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	if u, ok := access.ActorFrom(s.ActorContext(req)); !ok || !u.Local {
		t.Fatalf("primary actor: %+v %v", u, ok)
	}

	// Gated: sign in, then the cookie names the actor.
	rec := httptest.NewRecorder()
	login := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"name":"asha","password":"password-1"}`))
	login.Header.Set("Content-Type", "application/json")
	gatedHandler.ServeHTTP(rec, login)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	cookie := rec.Result().Cookies()[0]

	var actor access.User
	var ok bool
	probe := MarkGated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, ok = access.ActorFrom(s.ActorContext(r))
	}))
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.AddCookie(cookie)
	probe.ServeHTTP(httptest.NewRecorder(), r)
	if !ok || actor.Name != "asha" || actor.Local {
		t.Fatalf("gated actor: %+v %v", actor, ok)
	}

	// Gated without a sign-in: nobody.
	probe.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	if ok {
		t.Fatal("gated request without sign-in got an actor")
	}

	// Admin routes exist on the primary mux only.
	rec = httptest.NewRecorder()
	primary.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/access/users", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "asha") {
		t.Fatalf("admin on primary: %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	gatedHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/access/users", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("admin on gated mux: %d", rec.Code)
	}
}

// An approval recorded on the task with no note shown (a quiet one, an older image's untyped one) is gated too: the
// task record is read first (openApproval).
func TestGateFindsAnApprovalTheTaskRecordHolds(t *testing.T) {
	s, store := accessServer(t)
	if _, err := store.SavePolicy(access.Policy{ActionMatch: `\bpay\b`, ApproverRoles: []string{"finance"}}); err != nil {
		t.Fatal(err)
	}
	clerk, _ := store.CreateUser("clerk2", "password-1", nil)
	w := tasks.Worker{ID: "d2", Name: "d2"}
	route, err := s.Tasks.Plan(w.ID, "pay the vendor")
	if err != nil {
		t.Fatal(err)
	}
	if route, err = s.Tasks.Commit(route, w, "pay the vendor", tasks.Origin{Kind: tasks.OriginPerson}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Tasks.RecordOutcome(w.ID, route.TaskID, tasks.Outcome{Kind: "approval", Reply: "Approval needed before I click Pay: pays $900."}); err != nil {
		t.Fatal(err)
	}
	if action, ok := s.openApproval(route.TaskID); !ok || !strings.Contains(action, "Pay") {
		t.Fatalf("openApproval = %q %v", action, ok)
	}
	reply := tasks.Route{Kind: tasks.RouteReply, TaskID: route.TaskID}
	if err := s.gateApprovalReply(access.WithActor(context.Background(), clerk), "d2", reply, "yes"); err == nil {
		t.Fatal("a clerk's yes to an approval with no note was delivered")
	}
}
