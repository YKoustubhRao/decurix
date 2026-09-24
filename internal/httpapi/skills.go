package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/webui"
)

// A skill's name is checked here before it goes into a URL path on the
// desktop, against the pattern the desktop's skills.py enforces on its side.
var skillNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)

// maxLearnRecordings bounds how many demonstrations one lesson is taught from.
const maxLearnRecordings = 5

// skillsPageHandler renders one desktop's skills: the tasks it was taught by
// demonstration, each runnable with no model, and any lesson left half done.
// The page reads them from the desktop, which keeps them.
func (s *Server) skillsPageHandler(w http.ResponseWriter, r *http.Request) {
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/skills/"), "/")
	if id == "" {
		http.NotFound(w, r)
		return
	}

	found, err := s.Desktops.Get(r.Context(), id)
	if errors.Is(err, desktop.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		log.Printf("skills page: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if err := s.Web.RenderSkills(w, webui.SkillsPageData{Desktop: found}); err != nil {
		log.Printf("skills page: %v", err)
	}
}

// skillsAPIHandler lists the skills taught on a desktop.
func (s *Server) skillsAPIHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := requireDesktop(w, r, http.MethodGet)
	if !ok {
		return
	}

	s.callAgent(w, r, id, http.MethodGet, "/skills", nil, true)
}

// skillAPIHandler returns one skill with its steps.
func (s *Server) skillAPIHandler(w http.ResponseWriter, r *http.Request) {
	id, name, ok := requireSkill(w, r, http.MethodGet)
	if !ok {
		return
	}

	s.callAgent(w, r, id, http.MethodGet, "/skills/"+name, nil, true)
}

// skillExportHandler hands out a skill as a SKILL.md - the Agent Skills format, with
// the taught task's intent, its routes and its GUI replay hints - which any agent
// that reads skills can be given.
func (s *Server) skillExportHandler(w http.ResponseWriter, r *http.Request) {
	id, name, ok := requireSkill(w, r, http.MethodGet)
	if !ok {
		return
	}

	s.callAgent(w, r, id, http.MethodGet, "/skills/"+name+"/skill.md", nil, true)
}

// skillDeleteHandler forgets a skill. The recordings it was taught from are
// untouched.
func (s *Server) skillDeleteHandler(w http.ResponseWriter, r *http.Request) {
	id, name, ok := requireSkill(w, r, http.MethodPost)
	if !ok {
		return
	}

	s.callAgent(w, r, id, http.MethodPost, "/skills/"+name+"/delete", nil, true)
}

// runSkillHandler asks a desktop to do a skill it was taught, as a task.
//
// It goes through the task records exactly like a chat request - Plan, the
// desktop accepts it, Commit, and the chat shows it - so the run, and any
// question it stops to ask, is a task like any other. Unlike a replay's
// parameters, a skill's are never secret (a secret is a step a person does by
// hand), so they are part of the request the chat shows.
func (s *Server) runSkillHandler(w http.ResponseWriter, r *http.Request) {
	id, name, ok := requireSkill(w, r, http.MethodPost)
	if !ok {
		return
	}

	params, errText := parseReplayParams(r.FormValue("params"))
	if errText != "" {
		jsonStatusError(w, http.StatusBadRequest, errText)
		return
	}

	content, err := skillContent(name, params)
	if err != nil {
		jsonStatusError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.sendTaught(w, r, id, content, "/skills/"+name+"/run", map[string]any{"params": params})
}

// learnRecordingHandler starts a lesson: the desktop asks whoever recorded a
// demonstration what it needs to know to do the task itself. `session` is the
// recording, `sessions` optionally more of them, comma separated - the desktop
// then works out what differs between them. `name`, optional, is what the
// person calls the skill.
func (s *Server) learnRecordingHandler(w http.ResponseWriter, r *http.Request) {
	id, session, ok := requireRecording(w, r, http.MethodPost)
	if !ok {
		return
	}

	sessions := []string{session}
	for _, other := range strings.Split(r.FormValue("sessions"), ",") {
		other = strings.TrimSpace(other)
		if other == "" || other == session {
			continue
		}
		if !recordingIDPattern.MatchString(other) {
			jsonStatusError(w, http.StatusBadRequest, "not a recording id: "+other)
			return
		}
		sessions = append(sessions, other)
	}

	if len(sessions) > maxLearnRecordings {
		jsonStatusError(w, http.StatusBadRequest, fmt.Sprintf("teach from at most %d recordings at once", maxLearnRecordings))
		return
	}

	name := strings.Join(strings.Fields(r.FormValue("name")), " ")
	content, err := learnContent(sessions, name)
	if err != nil {
		jsonStatusError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.sendTaught(w, r, id, content, "/recordings/"+session+"/learn", map[string]any{"sessions": sessions[1:], "name": name})
}

// sendTaught delivers a "/skill" or "/learn" request to a desktop as a task:
// refused while the desktop is busy (sent to a paused agent the watcher would
// take it as the reply to whatever it asked), otherwise Plan, hand it to the
// desktop's own endpoint, Commit.
func (s *Server) sendTaught(w http.ResponseWriter, r *http.Request, id, content, agentPath string, payload map[string]any) {
	worker := desktop.SanitizeID(id)

	if why, err := s.Tasks.Busy(worker); err != nil {
		log.Printf("skills: %v", err)
	} else if why != "" {
		jsonStatusError(w, http.StatusConflict, why)
		return
	}

	route := s.routeRequest(worker, content)

	ctx, cancel := context.WithTimeout(r.Context(), recordingsRequestTimeout)
	defer cancel()

	payload["task_id"] = route.TaskID

	body, err := json.Marshal(payload)
	if err != nil {
		jsonError(w, err)
		return
	}

	resp, err := s.Desktops.AgentRequest(ctx, id, http.MethodPost, agentPath, bytes.NewReader(body))
	if err != nil {
		recordingsError(w, err)
		return
	}

	if resp.StatusCode != http.StatusOK {
		relayAgent(w, resp)
		return
	}
	resp.Body.Close()

	route = s.recordDelivered(r.Context(), worker, content, route)

	jsonResponse(w, map[string]string{"status": "sent", "task_id": route.TaskID, "content": content})
}

// maxSkillNameRunes bounds the name a person gives a skill they teach; the
// desktop keeps at most this much of it (http_api._learn).
const maxSkillNameRunes = 80

// learnContent is the request a lesson is sent and recorded as; the desktop's
// watcher routes on it (skill_task.parse_learn) and builds the same string
// itself (skill_task.learn_request). `name` is what the person called the
// skill, "" to let the desktop name it after the recording; a name holding
// both kinds of quote cannot be written into a request and is refused.
func learnContent(sessions []string, name string) (string, error) {
	content := "/learn " + strings.Join(sessions, " ")

	name = strings.Join(strings.Fields(name), " ")
	switch {
	case name == "":
		return content, nil
	case utf8.RuneCountInString(name) > maxSkillNameRunes:
		return "", fmt.Errorf("a skill name is at most %d characters", maxSkillNameRunes)
	case !strings.Contains(name, `"`):
		return fmt.Sprintf(`%s name="%s"`, content, name), nil
	case !strings.Contains(name, `'`):
		return fmt.Sprintf(`%s name='%s'`, content, name), nil
	default:
		return "", fmt.Errorf("the name has both kinds of quote in it")
	}
}

// skillContent is the request a skill run is sent and recorded as, and what
// the desktop's watcher reads back (skill_task.parse_skill): the name, then
// each parameter as name="value" in name order. A new line in a value is the
// same as ";" in a list. A value holding both kinds of quote cannot be written
// into a request and is refused rather than bent.
func skillContent(name string, params map[string]string) (string, error) {
	parts := []string{"/skill " + name}

	keys := make([]string, 0, len(params))
	for key := range params {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		var lines []string
		for _, line := range strings.Split(strings.ReplaceAll(params[key], "\r", ""), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				lines = append(lines, line)
			}
		}

		value := strings.Join(lines, ";")
		switch {
		case value == "":
			continue
		case !strings.Contains(value, `"`):
			parts = append(parts, fmt.Sprintf(`%s="%s"`, key, value))
		case !strings.Contains(value, `'`):
			parts = append(parts, fmt.Sprintf(`%s='%s'`, key, value))
		default:
			return "", fmt.Errorf("the value of %s has both kinds of quote in it", key)
		}
	}

	return strings.Join(parts, " "), nil
}

// requireSkill is requireDesktop plus a valid skill name.
func requireSkill(w http.ResponseWriter, r *http.Request, method string) (string, string, bool) {
	id, ok := requireDesktop(w, r, method)
	if !ok {
		return "", "", false
	}

	name := r.FormValue("name")
	if !skillNamePattern.MatchString(name) {
		jsonStatusError(w, http.StatusBadRequest, "not a skill name")
		return "", "", false
	}

	return id, name, true
}
