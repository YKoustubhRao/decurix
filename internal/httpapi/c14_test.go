package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// Whatever the records keep withheld reaches the agent marked secret (C14.1): a bare value answering a question
// whose words ask for a password, and a request that spells a labelled password, as well as the person's own mark.
// Text with nothing withheld carries no secret field.
func TestWhatTheRecordsWithholdReachesTheAgentAsSecret(t *testing.T) {
	s := chatServer(t)
	bodies := fakeAgentDesktop(t, s)
	sent := func() map[string]any {
		t.Helper()
		got := requestsWith(bodies(), "content")
		if len(got) == 0 {
			t.Fatal("nothing reached the agent")
		}
		return got[len(got)-1]
	}

	task := askedQuestion(t, s, tasks.OutcomeQuery, "I need the password for mira.k to sign in. What is it?")
	if rec := sendAs(t, s, url.Values{"id": {"box"}, "content": {"Qx7-tree-9"}}, false, ""); rec.Code != http.StatusOK {
		t.Fatalf("reply: %d %s", rec.Code, rec.Body)
	}
	if b := sent(); b["secret"] != true || b["content"] != "Qx7-tree-9" {
		t.Fatalf("a reply withheld by its question's words reached the agent as %v", b)
	}
	if j := onlyJudgement(t, s, task); strings.Contains(j.Text, "Qx7") {
		t.Fatalf("the records keep %q", j.Text)
	}
	if got := strings.Join(s.secretsSeen.For("box"), "|"); !strings.Contains(got, "Qx7-tree-9") {
		t.Fatalf("the call recorder does not know the value: %q", got)
	}
	_, _ = s.Tasks.RecordOutcome("box", task, tasks.Outcome{Kind: tasks.OutcomeDone, Reply: "signed in"})

	if rec := sendAs(t, s, url.Values{"id": {"box"}, "content": {"sign in to the staff portal, password: Plum7Orbit"}}, false, ""); rec.Code != http.StatusOK {
		t.Fatalf("request: %d %s", rec.Code, rec.Body)
	}
	if b := sent(); b["secret"] != true {
		t.Fatalf("a request the records withhold reached the agent as %v", b)
	}
	if rec := sendAs(t, s, url.Values{"id": {"box"}, "content": {"file the October bills"}}, false, ""); rec.Code != http.StatusOK {
		t.Fatalf("request: %d %s", rec.Code, rec.Body)
	}
	if b := sent(); b["secret"] != nil {
		t.Fatalf("a request with nothing withheld carries secret: %v", b)
	}
}

// The gated listener forwards no soul route but the read-only ones on its allowlist (C14.4, F-S1): soul's memory,
// skills, stats, chat and jobs answer 404 there, whatever method. The routes are read from soul's own mux.
func TestTheGatedListenerForwardsOnlyAllowedSoulRoutes(t *testing.T) {
	soulRoutes := literalPaths(t, filepath.Join("..", "..", "agent", "thebot", "cmd", "soul", "main.go"), "mux")
	if len(soulRoutes) < 6 {
		t.Fatalf("only %v read from soul's mux", soulRoutes)
	}
	_, gated := listenerMuxes(fullServer(t))
	h := MarkGated(gated)
	checked := 0
	for _, route := range append(soulRoutes, "/api/skills/x/confirm", "/api/jobs/j1", "/index.html", "/api/../api/memory") {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			if gatedAgentRoute(method, route) {
				continue
			}
			checked++
			req := httptest.NewRequest(method, "/agent/box"+route, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			// ServeMux answers a path to clean with a redirect to the clean one, which is refused in turn.
			if rec.Code != http.StatusNotFound && rec.Code != http.StatusMovedPermanently && rec.Code != http.StatusTemporaryRedirect {
				t.Errorf("%s /agent/box%s on the gated listener: %d, want 404", method, route, rec.Code)
			}
		}
	}
	if checked < 12 {
		t.Fatalf("only %d refusals checked", checked)
	}
	for p := range gatedAgentRoutes {
		if gatedAgentRoute(http.MethodPost, p) {
			t.Errorf("%s is forwarded for a write", p)
		}
	}
}
