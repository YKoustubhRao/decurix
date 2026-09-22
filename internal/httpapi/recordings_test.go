package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Everything a recordings call checks happens before it reaches a desktop, so
// a malformed or unsafe request never becomes a URL path on one. chatServer
// has no Docker manager: a test that got past the checks would panic.
func TestRecordingsRejectBadRequestsBeforeReachingADesktop(t *testing.T) {
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
		{"no desktop", get(s.recordingsAPIHandler, ""), http.StatusBadRequest},
		{"path in a session", get(s.recordingAPIHandler, "id=box&session=../etc"), http.StatusBadRequest},
		{"unknown session kind", get(s.recordingAPIHandler, "id=box&session=secret-1"), http.StatusBadRequest},
		{"path in a frame", get(s.recordingFrameHandler, "id=box&session=demo-1&frame=../../meta.json"), http.StatusBadRequest},
		{"post to a read", postForm(t, s.recordingAPIHandler, "/x", "id=box&session=demo-1"), http.StatusMethodNotAllowed},
		{"get to a write", get(s.recordingDeleteHandler, "id=box&session=demo-1"), http.StatusMethodNotAllowed},
		{"unknown action", postForm(t, s.demonstrationHandler, "/x", "id=box&action=erase"), http.StatusBadRequest},
		{"bad step", postForm(t, s.replayRecordingHandler, "/x", "id=box&session=demo-1&from=zero"), http.StatusBadRequest},
		{"params not an object", postForm(t, s.replayRecordingHandler, "/x", "id=box&session=demo-1&params=%5B1%5D"), http.StatusBadRequest},
		{"bad param name", postForm(t, s.replayRecordingHandler, "/x", "id=box&session=demo-1&params=%7B%22..%2Fx%22%3A%22a%22%7D"), http.StatusBadRequest},
		{"record with no task", postForm(t, s.recordTaskHandler, "/x", "id=box&task=++"), http.StatusBadRequest},
		{"record too long", postForm(t, s.recordTaskHandler, "/x", "id=box&task="+strings.Repeat("x", maxRequestBytes)), http.StatusBadRequest},
		{"record by get", get(s.recordTaskHandler, "id=box&task=x"), http.StatusMethodNotAllowed},
		{"demonstration status with no desktop", get(s.demonstrationHandler, ""), http.StatusBadRequest},
		{"ticket status with no desktop", get(s.ticketStatusHandler, ""), http.StatusBadRequest},
		{"ticket status by post", postForm(t, s.ticketStatusHandler, "/x", "id=box"), http.StatusMethodNotAllowed},
		{"unknown ticket action", postForm(t, s.ticketActionHandler, "/x", "id=box&action=cancel"), http.StatusBadRequest},
		{"ticket action by get", get(s.ticketActionHandler, "id=box&action=begin"), http.StatusMethodNotAllowed},
		{"ticket note with no text", postForm(t, s.ticketActionHandler, "/x", "id=box&action=note"), http.StatusBadRequest},
		{"ticket note with blank text", postForm(t, s.ticketActionHandler, "/x", "id=box&action=note&text=+++"), http.StatusBadRequest},
		{"import with no file", postForm(t, s.recordingImportHandler, "/x", "id=box"), http.StatusBadRequest},
		{"import that is not JSON", postForm(t, s.recordingImportHandler, "/x", "id=box&data=not+json"), http.StatusBadRequest},
		{"import that is a bare number", postForm(t, s.recordingImportHandler, "/x", "id=box&data=1"), http.StatusBadRequest},
		{"import too large", postForm(t, s.recordingImportHandler, "/x", "id=box&data=%7B%22a%22%3A%22"+strings.Repeat("x", maxImportBytes)+"%22%7D"), http.StatusBadRequest},
		{"import by get", get(s.recordingImportHandler, "id=box&data=%7B%7D"), http.StatusMethodNotAllowed},
		{"import with no desktop", postForm(t, s.recordingImportHandler, "/x", "data=%7B%7D"), http.StatusBadRequest},
	}

	for _, c := range cases {
		if c.rec.Code != c.want {
			t.Errorf("%s: status %d, want %d (%s)", c.name, c.rec.Code, c.want, c.rec.Body.String())
		}
	}
}

// A replay is refused while the desktop's agent has a task running or paused:
// the desktop would take it as the reply to the pause, and the records would
// describe a replay that never ran.
func TestReplayIsRefusedWhileTheAgentIsBusy(t *testing.T) {
	s := chatServer(t)

	deliver(t, s, "busy-box", "fill in the invoice form")

	rec := postForm(t, s.replayRecordingHandler, "/api/desktops/recordings/replay", "id=busy-box&session=demo-20260914T101500-abc123")

	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", rec.Code, rec.Body.String())
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || !strings.Contains(body["error"], "still running") {
		t.Fatalf("unexpected refusal: %s", rec.Body.String())
	}

	if why, err := s.Tasks.Busy("idle-box"); err != nil || why != "" {
		t.Fatalf("a desktop with no tasks is busy: %q %v", why, err)
	}

	// Pressing record is refused the same way: sent to a paused agent it
	// would be the reply, and a person would be recording nothing.
	rec = postForm(t, s.recordTaskHandler, "/api/desktops/record", "id=busy-box&task=fill+in+the+invoice+form")
	if rec.Code != http.StatusConflict {
		t.Fatalf("record: status %d, want 409: %s", rec.Code, rec.Body.String())
	}
}

// The request a replay is recorded as names the recording and the step, and
// never the values it was given.
func TestReplayContentCarriesNoParameterValues(t *testing.T) {
	if got := replayContent("demo-1", 1); got != "/replay demo-1" {
		t.Fatalf("replayContent = %q", got)
	}
	if got := replayContent("demo-1", 4); got != "/replay demo-1 from=4" {
		t.Fatalf("replayContent = %q", got)
	}

	params, errText := parseReplayParams(`{"password":"hunter2","customer_name":"Acme"}`)
	if errText != "" || params["password"] != "hunter2" {
		t.Fatalf("parseReplayParams: %v %q", params, errText)
	}
	if _, errText := parseReplayParams(`{"a":"` + strings.Repeat("x", maxReplayParamBytes+1) + `"}`); errText == "" {
		t.Fatal("an oversized value was accepted")
	}
}
