package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Everything a skills call checks happens before it reaches a desktop, so a
// malformed or unsafe request never becomes a URL path on one. chatServer has
// no Docker manager: a test that got past the checks would panic.
func TestSkillsRejectBadRequestsBeforeReachingADesktop(t *testing.T) {
	s := chatServer(t)

	get := func(handler http.HandlerFunc, query string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodGet, "/x?"+query, nil))
		return rec
	}

	cases := []struct {
		name string
		rec  *httptest.ResponseRecorder
		want int
	}{
		{"list with no desktop", get(s.skillsAPIHandler, ""), http.StatusBadRequest},
		{"path in a skill name", get(s.skillAPIHandler, "id=box&name=../etc"), http.StatusBadRequest},
		{"upper case skill name", get(s.skillAPIHandler, "id=box&name=Pay"), http.StatusBadRequest},
		{"no skill name", get(s.skillAPIHandler, "id=box"), http.StatusBadRequest},
		{"post to a read", postForm(t, s.skillAPIHandler, "/x", "id=box&name=pay"), http.StatusMethodNotAllowed},
		{"get to a delete", get(s.skillDeleteHandler, "id=box&name=pay"), http.StatusMethodNotAllowed},
		{"get to a run", get(s.runSkillHandler, "id=box&name=pay"), http.StatusMethodNotAllowed},
		{"run params not an object", postForm(t, s.runSkillHandler, "/x", "id=box&name=pay&params=%5B1%5D"), http.StatusBadRequest},
		{"run bad param name", postForm(t, s.runSkillHandler, "/x", "id=box&name=pay&params=%7B%22..%2Fx%22%3A%22a%22%7D"), http.StatusBadRequest},
		{"run value with both quotes", postForm(t, s.runSkillHandler, "/x", "id=box&name=pay&params=%7B%22a%22%3A%22%5C%22%27%22%7D"), http.StatusBadRequest},
		{"learn a bad recording", postForm(t, s.learnRecordingHandler, "/x", "id=box&session=secret-1"), http.StatusBadRequest},
		{"learn with a bad extra", postForm(t, s.learnRecordingHandler, "/x", "id=box&session=demo-1&sessions=demo-2,../x"), http.StatusBadRequest},
		{"learn from too many", postForm(t, s.learnRecordingHandler, "/x", "id=box&session=demo-1&sessions=demo-2,demo-3,demo-4,demo-5,demo-6"), http.StatusBadRequest},
		{"learn by get", get(s.learnRecordingHandler, "id=box&session=demo-1"), http.StatusMethodNotAllowed},
	}

	for _, c := range cases {
		if c.rec.Code != c.want {
			t.Errorf("%s: status %d, want %d (%s)", c.name, c.rec.Code, c.want, c.rec.Body.String())
		}
	}
}

// A lesson or a skill run is refused while the desktop's agent has a task
// running or paused: the desktop would take it as the reply to the pause, and
// the records would describe something that never happened.
func TestTeachingAndRunningAreRefusedWhileTheAgentIsBusy(t *testing.T) {
	s := chatServer(t)

	deliver(t, s, "busy-box", "fill in the invoice form")

	for name, rec := range map[string]*httptest.ResponseRecorder{
		"learn": postForm(t, s.learnRecordingHandler, "/x", "id=busy-box&session=demo-20260914T101500-abc123"),
		"run":   postForm(t, s.runSkillHandler, "/x", "id=busy-box&name=add-a-customer"),
	} {
		if rec.Code != http.StatusConflict {
			t.Errorf("%s: status %d, want 409: %s", name, rec.Code, rec.Body.String())
		}
	}
}

// The request a skill run is sent and recorded as is what the desktop's
// watcher reads back (skill_task.parse_skill): keep this table equal to
// agent/tests/test_skills.py's test_request_text_reads_back_as_written.
func TestSkillContentReadsBackAsWritten(t *testing.T) {
	got, err := skillContent("fill-an-order", map[string]string{"item": "Tea\nMilk\n", "note": `say "hi"`, "empty": " "})
	if err != nil {
		t.Fatal(err)
	}
	if want := `/skill fill-an-order item="Tea;Milk" note='say "hi"'`; got != want {
		t.Fatalf("skillContent = %q, want %q", got, want)
	}

	if got, _ := skillContent("add-a-customer", nil); got != "/skill add-a-customer" {
		t.Fatalf("skillContent with no params = %q", got)
	}

	if got, _ := skillContent("x", map[string]string{"b": "2", "a": "1"}); got != `/skill x a="1" b="2"` {
		t.Fatalf("parameters are not in name order: %q", got)
	}

	if _, err := skillContent("x", map[string]string{"a": `both " and '`}); err == nil || !strings.Contains(err.Error(), "both kinds of quote") {
		t.Fatalf("a value that cannot be written into a request was accepted: %v", err)
	}

	if got, _ := learnContent([]string{"demo-1", "demo-2"}, ""); got != "/learn demo-1 demo-2" {
		t.Fatalf("learnContent = %q", got)
	}
}

// Keep this table equal to agent/tests/test_skill_match.py's
// test_the_request_reads_back_the_same (skill_task.learn_request).
func TestLearnContentCarriesTheName(t *testing.T) {
	for name, want := range map[string]string{
		"Google a phrase":      `/learn demo-1 name="Google a phrase"`,
		"  Google   a phrase ": `/learn demo-1 name="Google a phrase"`,
		`Say "hi"`:             `/learn demo-1 name='Say "hi"'`,
	} {
		if got, err := learnContent([]string{"demo-1"}, name); err != nil || got != want {
			t.Errorf("learnContent(%q) = %q, %v; want %q", name, got, err, want)
		}
	}

	if _, err := learnContent([]string{"demo-1"}, `both " and '`); err == nil {
		t.Error("a name with both kinds of quote was accepted")
	}
	if _, err := learnContent([]string{"demo-1"}, strings.Repeat("x", maxSkillNameRunes+1)); err == nil {
		t.Error("an overlong name was accepted")
	}
}
