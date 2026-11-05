package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// A desktop's agent backs a pick from an application's list with a person's word only as the control plane recorded
// it (H-PY-18; agent/decurix_agent/person_record.py): /api/judgements is a person's to read, so these are the
// desktop's own doors to it. A desktop reads the judgements on its own tasks and only those, a secret one's text is
// never sent, and nothing here writes but the append-only learned row (rule 4). Primary listener only, and only for
// the desktop the request comes from (requestDesktop: its address on its own network): never the gated listener.

// maxSkillName bounds a skill named in a query.
const maxSkillName = 128

// desktopJudgement is one judgement as a desktop is given it.
type desktopJudgement struct {
	ID     string `json:"id"`
	TaskID string `json:"task_id"`
	Kind   string `json:"kind"`
	Actor  struct {
		Kind  string   `json:"kind"`
		Roles []string `json:"roles"`
	} `json:"actor"`
	Text string `json:"text"`
	// TextSHA256 is the SHA-256 (hex) of Text with its spacing collapsed (Python " ".join(text.split())): what the
	// agent confirms a reply it kept against. "" for a secret one.
	TextSHA256  string `json:"text_sha256"`
	Asked       string `json:"asked"`
	DerivedFrom string `json:"derived_from"`
	SkillName   string `json:"skill_name"`
	Secret      bool   `json:"secret"`
	At          string `json:"at"`
}

// collapsedSHA256 is the hex SHA-256 of text with its spacing collapsed to single spaces.
func collapsedSHA256(text string) string {
	sum := sha256.Sum256([]byte(strings.Join(strings.Fields(text), " ")))
	return hex.EncodeToString(sum[:])
}

// asDesktopJudgement is j as a desktop is given it: a secret one's text never.
func (s *Server) asDesktopJudgement(j tasks.Judgement) desktopJudgement {
	out := desktopJudgement{ID: j.ID, TaskID: j.TaskID, Kind: j.Kind, DerivedFrom: j.DerivedFrom, SkillName: j.SkillName,
		Secret: j.Secret, At: j.At.UTC().Format("2006-01-02T15:04:05.000Z07:00")}
	out.Actor.Kind, out.Actor.Roles = j.Actor.Kind, j.Actor.Roles
	if out.Actor.Roles == nil {
		out.Actor.Roles = []string{}
	}
	if !j.Secret {
		out.Text, out.TextSHA256 = j.Text, collapsedSHA256(j.Text)
		out.Asked = s.Tasks.InterventionRequest(j.InterventionID)
	}
	return out
}

// desktopJudgementsHandler answers GET /api/desktops/judgements?task=<id> (every judgement on one of the calling
// desktop's tasks; another's is 403) or ?skill=<name> (every judgement on each of its tasks that names that skill).
func (s *Server) desktopJudgementsHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	worker, fromDesktop := requestDesktop(r)
	if !fromDesktop {
		jsonStatusError(w, http.StatusForbidden, "only a desktop reads its own judgements here")
		return
	}
	task, skill := strings.TrimSpace(r.URL.Query().Get("task")), strings.TrimSpace(r.URL.Query().Get("skill"))
	var rows []tasks.Judgement
	var err error
	switch {
	case task != "" && skill == "":
		if !tasks.ValidID(task) {
			jsonStatusError(w, http.StatusBadRequest, "name a task")
			return
		}
		if owner, ok := s.Tasks.TaskWorker(task); !ok || owner != worker {
			jsonStatusError(w, http.StatusForbidden, "a desktop reads the judgements on its own tasks only")
			return
		}
		rows, err = s.Tasks.JudgementsForTask(task)
	case skill != "" && task == "" && len(skill) <= maxSkillName:
		rows, err = s.Tasks.JudgementsForSkill(worker, skill)
	default:
		jsonStatusError(w, http.StatusBadRequest, "name a task or a skill")
		return
	}
	if err != nil {
		internalError(w, "desktop judgements: reading them", err)
		return
	}
	out := []desktopJudgement{}
	for _, j := range rows {
		if j.WorkerID == worker { // never another desktop's row, whatever task it names
			out = append(out, s.asDesktopJudgement(j))
		}
	}
	jsonResponse(w, map[string]any{"judgements": out})
}

// desktopLearnedRow is one learned row as its desktop reads it back, with the judgement it was derived from.
type desktopLearnedRow struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	TaskID      string `json:"task_id"`
	Text        string `json:"text"`
	DerivedFrom string `json:"derived_from"`
	Source      struct {
		Actor struct {
			Kind string `json:"kind"`
		} `json:"actor"`
		Text string `json:"text"`
	} `json:"source"`
}

// desktopLearnedList answers GET /api/desktops/learned?kind=field_answer: the calling desktop's rows of that kind,
// each with its source judgement (a secret one's text never). Only its own rows: per worker, not the org's.
func (s *Server) desktopLearnedList(w http.ResponseWriter, r *http.Request, worker string) {
	if kind := r.URL.Query().Get("kind"); kind != tasks.JudgementFieldAnswer {
		jsonStatusError(w, http.StatusBadRequest, "kind must be field_answer")
		return
	}
	rows, err := s.Tasks.JudgementsOfKind(worker, tasks.JudgementFieldAnswer)
	if err != nil {
		internalError(w, "learned: reading them", err)
		return
	}
	out := []desktopLearnedRow{}
	for _, j := range rows {
		if j.WorkerID != worker {
			continue
		}
		row := desktopLearnedRow{ID: j.ID, Kind: j.Kind, TaskID: j.TaskID, DerivedFrom: j.DerivedFrom}
		if !j.Secret {
			row.Text = j.Text
		}
		if source, ok, err := s.Tasks.Judgement(j.DerivedFrom); err == nil && ok && source.WorkerID == worker {
			row.Source.Actor.Kind = source.Actor.Kind
			if !source.Secret {
				row.Source.Text = source.Text
			}
		}
		out = append(out, row)
	}
	jsonResponse(w, map[string]any{"learned": out})
}

// fieldAnswer is a field_answer row's text: the field (its label, on its host) and the option the person chose.
type fieldAnswer struct {
	Label  string `json:"label"`
	Host   string `json:"host"`
	Option string `json:"option"`
}

// checkFieldAnswer reports whether text is a field_answer whose option stands in source (the person's judgement it
// is derived from), its spacing collapsed on both sides: a row that does not name what the person said is refused.
func checkFieldAnswer(text, source string) (ok bool, why string) {
	var fa fieldAnswer
	if err := json.Unmarshal([]byte(text), &fa); err != nil || strings.TrimSpace(fa.Label) == "" ||
		strings.TrimSpace(fa.Option) == "" || len(fa.Label) > 500 || len(fa.Host) > 255 || len(fa.Option) > 500 {
		return false, "a field_answer's text is {\"label\", \"host\", \"option\"}"
	}
	option := strings.Join(strings.Fields(fa.Option), " ")
	if !strings.Contains(strings.Join(strings.Fields(source), " "), option) {
		return false, "the option is not in what the person said"
	}
	return true, ""
}
