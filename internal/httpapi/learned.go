package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/YKoustubhRao/decurix/internal/llmrec"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// maxLearnedBytes bounds one learned row: a correction, a run answer or a lesson is a few sentences.
const maxLearnedBytes = 32 << 10

// desktopLearnedHandler keeps on the control plane what a desktop kept from a person's reply, when it keeps it (plan
// R8, C7): a skill correction, a run answer, a lesson. Deleting or recreating the desktop then loses none of them.
//
// The row is a derived judgement. Its author is the person who said it, taken from the judgement it cites
// (derived_from), never from the desktop: that judgement must be a person's, on this task, of this desktop (409
// otherwise). A desktop posts only for its own tasks (403), and only a desktop posts here (primary listener). A
// field_answer (H-PY-18) is kept only when its option stands in that judgement's text, and a field_unit only when that
// judgement is the person's numbered reply to the save's unit question for that very field (checkFieldUnit); GET
// ?kind=field_answer or ?kind=field_unit reads the desktop's own such rows back (desktopLearnedList).
func (s *Server) desktopLearnedHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && !allowMethod(w, r, http.MethodPost) {
		return
	}
	worker, fromDesktop := requestDesktop(r)
	if !fromDesktop {
		jsonStatusError(w, http.StatusForbidden, "only a desktop keeps what it learned here")
		return
	}
	if r.Method == http.MethodGet {
		s.desktopLearnedList(w, r, worker)
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
	case tasks.JudgementCorrection, tasks.JudgementRunAnswer, tasks.JudgementLesson, tasks.JudgementFieldAnswer,
		tasks.JudgementFieldUnit:
	default:
		jsonStatusError(w, http.StatusBadRequest, "kind must be correction, run_answer, lesson, field_answer or field_unit")
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
	// A kept rule stands on someone known: not on an input whose author holds no role (someone with the access code,
	// not signed in), and only on the person's own input, never on another derived row.
	if len(source.Actor.Roles) == 0 {
		jsonStatusError(w, http.StatusConflict, "that judgement's author holds no role, so nothing is kept from it")
		return
	}
	if source.DerivedFrom != "" {
		jsonStatusError(w, http.StatusConflict, "cite the person's own judgement, not one derived from it")
		return
	}
	if body.Kind == tasks.JudgementFieldAnswer || body.Kind == tasks.JudgementFieldUnit {
		if source.Secret {
			jsonStatusError(w, http.StatusConflict, "nothing is kept from a secret answer")
			return
		}
		check := checkFieldAnswer
		if body.Kind == tasks.JudgementFieldUnit {
			check = checkFieldUnit
		}
		if ok, why := check(text, source.Text, s.Tasks.InterventionRequest(source.InterventionID)); !ok {
			jsonStatusError(w, http.StatusConflict, why)
			return
		}
	}
	// Withheld with what this desktop's records withheld (the values its person's replies held) before withhold.Text:
	// the table is append-only, so a value that got in could never be taken out.
	held := s.heldFor(worker)
	text = llmrec.WithholdValues(text, held)
	payload, _ := json.Marshal(map[string]string{"question": llmrec.WithholdValues(body.Question, held)})
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
