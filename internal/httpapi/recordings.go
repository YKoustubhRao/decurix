package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/tasks"
	"github.com/YKoustubhRao/decurix/internal/webui"
)

// A recording's ID and a frame's name are checked here, before either goes
// into a URL path on the desktop, against the same patterns the desktop's
// recording.py enforces on its side.
var (
	recordingIDPattern = regexp.MustCompile(`^(task|adhoc|demo)-[A-Za-z0-9-]{1,64}$`)
	frameNamePattern   = regexp.MustCompile(`^[0-9a-f]{20}\.png$`)
	paramNamePattern   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
)

const (
	// recordingsRequestTimeout bounds every call to a desktop's recordings API
	// except an export, which streams for as long as the recording is large.
	recordingsRequestTimeout = 60 * time.Second

	maxReplayParams     = 50
	maxReplayParamBytes = 4096
)

// demonstrationActions are what the recordings page can ask of a desktop's
// demonstration recorder.
var demonstrationActions = map[string]bool{"start": true, "pause": true, "resume": true, "stop": true}

// ticketActions are what the GUI screen's toolbar can ask of the ticket open
// on a desktop.
var ticketActions = map[string]bool{"begin": true, "pause": true, "resume": true, "note": true, "end": true}

// recordingsPageHandler renders one desktop's recordings: demonstrations
// recorded on it and the tasks its agent worked, each replayable and
// exportable. The page reads the recordings from the desktop itself.
func (s *Server) recordingsPageHandler(w http.ResponseWriter, r *http.Request) {
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/recordings/"), "/")
	if id == "" {
		http.NotFound(w, r)
		return
	}

	found, ok := s.pageDesktop(w, r, id, "recordings page")
	if !ok {
		return
	}

	session := r.URL.Query().Get("session")
	if !recordingIDPattern.MatchString(session) {
		session = ""
	}

	if err := s.Web.RenderRecordings(w, webui.RecordingsPageData{Desktop: found, Session: session}); err != nil {
		log.Printf("recordings page: %v", err)
	}
}

// recordingsAPIHandler lists a desktop's recordings and the demonstration
// being recorded on it, if any.
func (s *Server) recordingsAPIHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := requireDesktop(w, r, http.MethodGet)
	if !ok {
		return
	}

	s.callAgent(w, r, id, http.MethodGet, "/recordings", nil, true)
}

// recordingAPIHandler returns one recording: its meta, its events, and the
// program a replay of it would run.
func (s *Server) recordingAPIHandler(w http.ResponseWriter, r *http.Request) {
	id, session, ok := requireRecording(w, r, http.MethodGet)
	if !ok {
		return
	}

	s.callAgent(w, r, id, http.MethodGet, "/recordings/"+session, nil, true)
}

// recordingFrameHandler serves one full-size frame of a recording.
func (s *Server) recordingFrameHandler(w http.ResponseWriter, r *http.Request) {
	id, session, ok := requireRecording(w, r, http.MethodGet)
	if !ok {
		return
	}

	frame := r.URL.Query().Get("frame")
	if !frameNamePattern.MatchString(frame) {
		jsonStatusError(w, http.StatusBadRequest, "not a frame of a recording")
		return
	}

	s.callAgent(w, r, id, http.MethodGet, "/recordings/"+session+"/frames/"+frame, nil, true)
}

// recordingExportHandler streams a recording as a zip: meta.json,
// events.jsonl, program.json and its frames.
func (s *Server) recordingExportHandler(w http.ResponseWriter, r *http.Request) {
	id, session, ok := requireRecording(w, r, http.MethodGet)
	if !ok {
		return
	}

	s.callAgent(w, r, id, http.MethodGet, "/recordings/"+session+"/export", nil, false)
}

// recordingUpdateHandler renames a recording or changes its description.
func (s *Server) recordingUpdateHandler(w http.ResponseWriter, r *http.Request) {
	id, session, ok := requireRecording(w, r, http.MethodPost)
	if !ok {
		return
	}

	payload := map[string]string{}
	for _, field := range []string{"title", "description"} {
		if values, present := r.PostForm[field]; present && len(values) > 0 {
			payload[field] = values[0]
		}
	}

	s.callAgent(w, r, id, http.MethodPost, "/recordings/"+session+"/update", payload, true)
}

// maxImportBytes bounds a recording uploaded to be imported: the desktop's
// agent reads one request body of at most a mebibyte, and the export is only
// part of it.
const maxImportBytes = 900 * 1024

// recordingImportHandler saves a recording made somewhere else (a Chrome
// DevTools Recorder export, a BrowserGym trajectory or an OpenHands event list, as JSON) on a desktop as a demonstration of its
// own, which it can then replay, teach from and lint like one recorded there.
// The file is only checked for being a JSON object here; what is in it is the
// agent's to read (computer/ingest.py), and its reasons for refusing one reach
// the person as they are.
func (s *Server) recordingImportHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := requireDesktop(w, r, http.MethodPost)
	if !ok {
		return
	}

	raw := strings.TrimSpace(r.FormValue("data"))

	switch {
	case raw == "":
		jsonStatusError(w, http.StatusBadRequest, "choose a recording to import")
		return
	case len(raw) > maxImportBytes:
		jsonStatusError(w, http.StatusBadRequest, "that recording is too large to import")
		return
	case (raw[0] != '{' && raw[0] != '[') || !json.Valid([]byte(raw)):
		// An object (a Chrome Recorder export, a BrowserGym trajectory) or a list (an
		// OpenHands trajectory's events); which one it is is the agent's to say.
		jsonStatusError(w, http.StatusBadRequest, "that is not a JSON recording")
		return
	}

	title := strings.TrimSpace(r.FormValue("title"))
	if len(title) > 200 {
		title = title[:200]
	}

	s.callAgent(w, r, id, http.MethodPost, "/recordings/import", map[string]any{"title": title, "data": json.RawMessage(raw)}, true)
}

// recordingDeleteHandler deletes a recording from its desktop.
func (s *Server) recordingDeleteHandler(w http.ResponseWriter, r *http.Request) {
	id, session, ok := requireRecording(w, r, http.MethodPost)
	if !ok {
		return
	}

	s.callAgent(w, r, id, http.MethodPost, "/recordings/"+session+"/delete", map[string]string{}, true)
}

// demonstrationHandler reads (GET) or starts, pauses, resumes or stops (POST)
// recording a person's demonstration on a desktop.
func (s *Server) demonstrationHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if id, ok := requireDesktop(w, r, http.MethodGet); ok {
			s.callAgent(w, r, id, http.MethodGet, "/demonstration", nil, true)
		}
		return
	}

	id, ok := requireDesktop(w, r, http.MethodPost)
	if !ok {
		return
	}

	action := r.FormValue("action")
	if !demonstrationActions[action] {
		jsonStatusError(w, http.StatusBadRequest, "action must be start, pause, resume or stop")
		return
	}

	payload := map[string]string{
		"title":       r.FormValue("title"),
		"description": r.FormValue("description"),
		"reason":      r.FormValue("reason"),
	}

	s.callAgent(w, r, id, http.MethodPost, "/demonstration/"+action, payload, true)
}

// recordTaskHandler is the GUI screen toolbar's + : it sends "/record <task>"
// to a desktop, which opens a ticket for a person to do that task rather than
// handing it to the desktop's engine (record_task.py). Opening a ticket does
// not itself start capturing - the toolbar's Record does that, through
// ticketActionHandler("begin") - so a person can create the ticket, then
// start recording only once they are actually at the screen and ready.
//
// It is a chat request in every other respect - Plan, the desktop accepts it,
// Commit, the chat shows it - so the ticket and the human's steps land in a
// task of their own. Refused while the agent is busy, for the reason a replay
// is: sent to a paused agent it would be taken as the reply.
func (s *Server) recordTaskHandler(w http.ResponseWriter, r *http.Request) {
	limitUpload(w, r)

	files, err := readAttachments(r)
	if err != nil {
		uploadError(w, err)
		return
	}

	id, ok := requireDesktop(w, r, http.MethodPost)
	if !ok {
		return
	}

	// One line: what follows the task's line is the files it was given.
	task := strings.Join(strings.Fields(r.FormValue("task")), " ")
	if task == "" {
		jsonStatusError(w, http.StatusBadRequest, "say what task you are about to do")
		return
	}

	content := tasks.RecordCommand + " " + task
	if len(content) > maxRequestBytes {
		jsonStatusError(w, http.StatusBadRequest, "request too long")
		return
	}

	worker := desktop.SanitizeID(id)

	// Held until the request is handed over, so no automated send can
	// slip in between this busy check and it (see sendlock.go).
	unlock := s.lockDesktop(worker)
	defer unlock()

	if s.refuseBusy(w, worker, "record") {
		return
	}

	// The documents the person will work from (a PO to key in, an invoice to
	// file): kept with the ticket's recording, so a lesson taught from it
	// reads them beside what was typed (record_task.py, study.py).
	paths, err := s.putAttachments(r.Context(), worker, files)
	if err != nil {
		uploadError(w, err)
		return
	}

	content = withAttachments(content, paths)
	if len(content) > maxRequestBytes {
		jsonStatusError(w, http.StatusBadRequest, "request too long")
		return
	}

	route := s.routeRequest(worker, content)

	if err := s.deliverRequest(r.Context(), id, content, route.TaskID); err != nil {
		sendRequestError(w, err)
		return
	}

	route = s.recordDelivered(r.Context(), worker, content, route)

	jsonResponse(w, map[string]string{"status": "sent", "task_id": route.TaskID, "content": content})
}

// ticketAttachHandler adds files to the ticket open on a desktop (POST
// /api/desktops/ticket/attach, multipart: id + files): a document the person
// reaches for part way through, kept with the ticket's recording like the
// ones it was opened with.
func (s *Server) ticketAttachHandler(w http.ResponseWriter, r *http.Request) {
	limitUpload(w, r)

	files, err := readAttachments(r)
	if err != nil {
		uploadError(w, err)
		return
	}

	id, ok := requireDesktop(w, r, http.MethodPost)
	if !ok {
		return
	}

	if len(files) == 0 {
		jsonStatusError(w, http.StatusBadRequest, "choose a file to attach")
		return
	}

	paths, err := s.putAttachments(r.Context(), desktop.SanitizeID(id), files)
	if err != nil {
		uploadError(w, err)
		return
	}

	s.callAgent(w, r, id, http.MethodPost, "/ticket/attach", map[string]any{"files": paths}, true)
}

// ticketStatusHandler reads the ticket open on a desktop, if any: the GUI
// screen toolbar's poll, so every open tab (and the recordings page) agrees
// on whether it is armed, recording, or paused.
func (s *Server) ticketStatusHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := requireDesktop(w, r, http.MethodGet)
	if !ok {
		return
	}

	s.callAgent(w, r, id, http.MethodGet, "/ticket", nil, true)
}

// ticketActionHandler begins, pauses, resumes, notes or ends capturing for
// the ticket open on a desktop - the GUI screen toolbar's Record/Stop/
// Continue/Reason/End. begin/pause/resume are the same demonstration.py
// primitives the recordings page's own ad-hoc recorder uses; note adds a
// person's own written explanation to the recording at this moment; end is
// what finishes the ticket's task (record_task.end), which is what makes its
// recording's export downloadable.
func (s *Server) ticketActionHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := requireDesktop(w, r, http.MethodPost)
	if !ok {
		return
	}

	action := r.FormValue("action")
	if !ticketActions[action] {
		jsonStatusError(w, http.StatusBadRequest, "action must be begin, pause, resume, note or end")
		return
	}

	// callAgent tests its payload against the untyped nil literal, so a nil
	// map boxed into the `any` parameter would not compare equal to it and
	// would be marshalled as the JSON literal null instead of no body at all
	// - pass nil itself for the actions that take no fields.
	switch action {
	case "pause":
		s.callAgent(w, r, id, http.MethodPost, "/ticket/pause", map[string]string{"reason": r.FormValue("reason")}, true)
		return
	case "note":
		text := strings.TrimSpace(r.FormValue("text"))
		if text == "" {
			jsonStatusError(w, http.StatusBadRequest, "say what you're doing")
			return
		}
		s.callAgent(w, r, id, http.MethodPost, "/ticket/note", map[string]string{"text": text}, true)
		return
	}

	s.callAgent(w, r, id, http.MethodPost, "/ticket/"+action, nil, true)
}

// replayRecordingHandler runs a recording again on its desktop, as a task.
//
// It goes through the task records exactly like a chat request - Plan, the
// desktop accepts it, Commit, and the chat shows it - so the replay's steps,
// pauses and outcome land in a task like any other. What the chat and the
// records see is replayContent: the recording and the step. The parameter
// values travel only in the request to the desktop, which keeps them in
// memory, so a password given to a replay is written down nowhere.
func (s *Server) replayRecordingHandler(w http.ResponseWriter, r *http.Request) {
	id, session, ok := requireRecording(w, r, http.MethodPost)
	if !ok {
		return
	}

	from := 1
	if value := r.FormValue("from"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 {
			jsonStatusError(w, http.StatusBadRequest, "from must be a step number")
			return
		}
		from = n
	}

	params, errText := parseReplayParams(r.FormValue("params"))
	if errText != "" {
		jsonStatusError(w, http.StatusBadRequest, errText)
		return
	}

	s.sendTaught(w, r, id, replayContent(session, from), "/recordings/"+session+"/replay", map[string]any{"from": from, "params": params}, "replay")
}

// replayContent is the request a replay is sent and recorded as. The desktop's
// watcher routes on it (replay_task.parse) and the chat shows it; it names the
// recording and the step to start at, never a parameter's value.
func replayContent(session string, from int) string {
	if from > 1 {
		return "/replay " + session + " from=" + strconv.Itoa(from)
	}
	return "/replay " + session
}

// parseReplayParams reads a replay's parameters, a JSON object of name to
// text, and returns an explanation for the person when they are not usable.
func parseReplayParams(raw string) (map[string]string, string) {
	params := map[string]string{}

	if strings.TrimSpace(raw) == "" {
		return params, ""
	}

	if err := json.Unmarshal([]byte(raw), &params); err != nil {
		return nil, "params must be a JSON object of parameter names to text"
	}

	if len(params) > maxReplayParams {
		return nil, "too many parameters"
	}

	for name, value := range params {
		if !paramNamePattern.MatchString(name) {
			return nil, "not a parameter name: " + name
		}
		if len(value) > maxReplayParamBytes {
			return nil, "the value of " + name + " is too long"
		}
	}

	return params, ""
}

// requireDesktop checks the method and reads the desktop ID a recordings call
// is for.
func requireDesktop(w http.ResponseWriter, r *http.Request, method string) (string, bool) {
	if !requireMethod(w, r, method) {
		return "", false
	}

	id := strings.TrimSpace(r.FormValue("id"))
	if id == "" {
		jsonStatusError(w, http.StatusBadRequest, "missing desktop id")
		return "", false
	}

	return id, true
}

// requireNamed is requireDesktop plus the form value field, which must match
// pattern; refusal is the answer when it does not.
func requireNamed(w http.ResponseWriter, r *http.Request, method, field string, pattern *regexp.Regexp, refusal string) (string, string, bool) {
	id, ok := requireDesktop(w, r, method)
	if !ok {
		return "", "", false
	}

	name := r.FormValue(field)
	if !pattern.MatchString(name) {
		jsonStatusError(w, http.StatusBadRequest, refusal)
		return "", "", false
	}

	return id, name, true
}

// requireRecording is requireDesktop plus a valid recording ID.
func requireRecording(w http.ResponseWriter, r *http.Request, method string) (string, string, bool) {
	return requireNamed(w, r, method, "session", recordingIDPattern, "not a recording id")
}

// callAgent sends one call to a desktop's recordings API and relays the
// answer. `bounded` puts recordingsRequestTimeout on it.
func (s *Server) callAgent(w http.ResponseWriter, r *http.Request, id, method, path string, payload any, bounded bool) {
	ctx := r.Context()

	if bounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, recordingsRequestTimeout)
		defer cancel()
	}

	if resp, ok := s.askAgent(ctx, w, id, method, path, payload); ok {
		relayAgent(w, resp)
	}
}

// askAgent sends one call to a desktop's agent, payload (unless nil) as its
// JSON body, and returns the answer; a call that could not be made is answered
// here and returns false.
func (s *Server) askAgent(ctx context.Context, w http.ResponseWriter, id, method, path string, payload any) (*http.Response, bool) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			jsonError(w, err)
			return nil, false
		}
		body = bytes.NewReader(encoded)
	}

	resp, err := s.Desktops.AgentRequest(ctx, id, method, path, body)
	if err != nil {
		recordingsError(w, err)
		return nil, false
	}

	return resp, true
}

// refuseBusy answers 409 with why desktop worker is busy - sent to a paused
// agent, a request would be taken as the reply to whatever it asked - and
// reports whether it did. A records error is logged under what and refuses
// nothing.
func (s *Server) refuseBusy(w http.ResponseWriter, worker, what string) bool {
	why, err := s.Tasks.Busy(worker)
	if err != nil {
		log.Printf("%s: %v", what, err)
		return false
	}

	if why != "" {
		jsonStatusError(w, http.StatusConflict, why)
		return true
	}

	return false
}

// relayAgent copies a desktop agent's answer to the client: its status, the
// headers that describe the body, and the body, streamed. A 4xx carries words
// the agent wrote for a person ("stop the demonstration first") and is passed
// on; a 5xx can carry an exception's text, which jsonError's rule keeps from
// the client, so it is logged and replaced.
func relayAgent(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusInternalServerError {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		log.Printf("desktop agent returned %d: %s", resp.StatusCode, detail)
		jsonStatusError(w, http.StatusBadGateway, "the desktop could not do that - its agent log has the details")
		return
	}

	for _, name := range []string{"Content-Type", "Content-Length", "Content-Disposition", "Cache-Control"} {
		if value := resp.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	// The body is the desktop's, served from the control plane's own address: never run as one of its pages
	// (review 2026-11-02 - a desktop answering with text/html had the dashboard's rights).
	w.Header().Set("Content-Security-Policy", containerContentPolicy)
	w.Header().Set("X-Content-Type-Options", "nosniff")

	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// recordingsError maps a failure to reach a desktop's agent to a status and
// words a person can act on.
func recordingsError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, desktop.ErrNoNetwork), errors.Is(err, desktop.ErrNoAddress):
		log.Printf("recordings: %v", err)
		jsonStatusError(w, http.StatusServiceUnavailable, "the desktop is not reachable on the Decurix network")
	case errors.Is(err, context.DeadlineExceeded):
		jsonStatusError(w, http.StatusGatewayTimeout, "the desktop took too long to answer")
	default:
		sendRequestError(w, err)
	}
}
