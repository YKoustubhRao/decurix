package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/YKoustubhRao/decurix/internal/tasks"
	"github.com/YKoustubhRao/decurix/internal/withhold"
)

// maxLearnedBytes bounds one learned row: a correction, a run answer or a lesson is a few sentences.
const maxLearnedBytes = 32 << 10

// desktopLearnedHandler keeps on the control plane what a desktop kept from a person's reply, when it keeps it (plan
// R8, C7): a skill correction, a run answer, a lesson. Deleting or recreating the desktop then loses none of them.
//
// The row is a derived judgement. Its author is the person who said it, taken from the judgement it cites
// (derived_from), never from the desktop: that judgement must be a person's, on this task, of this desktop (409
// otherwise). A desktop posts only for its own tasks (403), and only a desktop posts here (primary listener).
func (s *Server) desktopLearnedHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodPost) {
		return
	}
	worker, fromDesktop := requestDesktop(r)
	if !fromDesktop {
		jsonStatusError(w, http.StatusForbidden, "only a desktop keeps what it learned here")
		return
	}
	var body struct {
		TaskID       string `json:"task_id"`
		Kind         string `json:"kind"`
		Skill        string `json:"skill"`
		SkillVersion int    `json:"skill_version"`
		DerivedFrom  string `json:"derived_from"`
		Question     string `json:"question"`
		Text         string `json:"text"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxLearnedBytes)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		badJSON(w, err)
		return
	}
	switch body.Kind {
	case tasks.JudgementCorrection, tasks.JudgementRunAnswer, tasks.JudgementLesson:
	default:
		jsonStatusError(w, http.StatusBadRequest, "kind must be correction, run_answer or lesson")
		return
	}
	text := strings.TrimSpace(body.Text)
	if !tasks.ValidID(body.TaskID) || !tasks.ValidJudgementID(body.DerivedFrom) || text == "" ||
		len(body.Skill) > 128 || body.SkillVersion < 0 || len(body.Question) > 4000 || len(text) > 8000 {
		jsonStatusError(w, http.StatusBadRequest, "say the task, the judgement it came from and what was kept")
		return
	}
	if owner, ok := s.Tasks.TaskWorker(body.TaskID); !ok || owner != worker {
		jsonStatusError(w, http.StatusForbidden, "a desktop keeps what it learned on its own tasks only")
		return
	}
	source, found, err := s.Tasks.Judgement(body.DerivedFrom)
	if err != nil {
		internalError(w, "learned: reading the judgement it came from", err)
		return
	}
	if !found || source.TaskID != body.TaskID || source.WorkerID != worker || source.Actor.Kind != tasks.ActorPerson {
		jsonStatusError(w, http.StatusConflict, "that is not a person's judgement on this task")
		return
	}
	payload, _ := json.Marshal(map[string]string{"question": withhold.Text(body.Question)})
	id, err := s.Tasks.AddJudgement(tasks.Judgement{
		Actor: source.Actor, Door: "derived", Kind: body.Kind, WorkerID: worker, TaskID: body.TaskID,
		ExecutionID: source.ExecutionID, InterventionID: source.InterventionID, SkillName: body.Skill,
		SkillVersion: body.SkillVersion, Text: text, Secret: source.Secret, Payload: payload, DerivedFrom: source.ID,
	})
	if err != nil {
		internalError(w, "learned: keeping it", err)
		return
	}
	jsonResponseStatus(w, http.StatusCreated, map[string]string{"id": id})
}
