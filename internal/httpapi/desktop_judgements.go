package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
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
		// Asked is the question the source judgement answered: the agent backs a pick with the row only when the
		// row's label stands in it (H-PY-19). "" for a secret source.
		Asked string `json:"asked"`
	} `json:"source"`
}

// desktopLearnedList answers GET /api/desktops/learned?kind=field_answer (or field_unit): the calling desktop's rows of
// that kind, each with its source judgement (a secret one's text never). Only its own rows: per worker, not the org's.
func (s *Server) desktopLearnedList(w http.ResponseWriter, r *http.Request, worker string) {
	kind := r.URL.Query().Get("kind")
	if kind != tasks.JudgementFieldAnswer && kind != tasks.JudgementFieldUnit {
		jsonStatusError(w, http.StatusBadRequest, "kind must be field_answer or field_unit")
		return
	}
	rows, err := s.Tasks.JudgementsOfKind(worker, kind)
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
				row.Source.Asked = s.Tasks.InterventionRequest(source.InterventionID)
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

// The words of a field's label, as the agent reads them (agent/decurix_agent/computer/records.py label_words): runs of
// lowercase letters, digits, "/" and "#", each synonym as its word, filler dropped. Kept the same on both sides, so
// the control plane never keeps a row the agent's read would refuse, nor refuses one it would take.
var (
	labelWordPattern = regexp.MustCompile(`[a-z0-9/#]+`)
	labelFiller      = map[string]bool{"of": true, "the": true, "a": true, "an": true, "in": true, "rs": true, "inr": true,
		"usd": true, "per": true, "dd": true, "mm": true, "yyyy": true, "yy": true, "and": true}
	labelSynonyms = map[string]string{"no": "number", "num": "number", "#": "number", "nbr": "number", "ref": "reference",
		"amt": "amount", "qty": "quantity", "dt": "date", "a/c": "account", "ac": "account"}
)

// labelWords is s's label words (above).
func labelWords(s string) []string {
	var out []string
	for _, w := range labelWordPattern.FindAllString(strings.ToLower(s), -1) {
		if syn, ok := labelSynonyms[w]; ok {
			w = syn
		}
		if !labelFiller[w] {
			out = append(out, w)
		}
	}
	return out
}

// labelInQuestion reports whether every word of label stands, as a whole word, in the question asked: an answer
// about one field is never kept for another (H-PY-19).
func labelInQuestion(label, asked string) bool {
	words := labelWords(label)
	if len(words) == 0 {
		return false
	}
	have := map[string]bool{}
	for _, w := range labelWords(asked) {
		have[w] = true
	}
	for _, w := range words {
		if !have[w] {
			return false
		}
	}
	return true
}

// checkFieldAnswer reports whether text is a field_answer whose option stands in source (the person's judgement it
// is derived from), its spacing collapsed on both sides, and whose label names a field the question it answered
// (asked) names: a row that does not stand on what the person was asked and said is refused.
func checkFieldAnswer(text, source, asked string) (ok bool, why string) {
	var fa fieldAnswer
	if err := json.Unmarshal([]byte(text), &fa); err != nil || strings.TrimSpace(fa.Label) == "" ||
		strings.TrimSpace(fa.Option) == "" || len(fa.Label) > 500 || len(fa.Host) > 255 || len(fa.Option) > 500 {
		return false, "a field_answer's text is {\"label\", \"host\", \"option\"}"
	}
	option := strings.Join(strings.Fields(fa.Option), " ")
	if !strings.Contains(strings.Join(strings.Fields(source), " "), option) {
		return false, "the option is not in what the person said"
	}
	if !labelInQuestion(fa.Label, asked) {
		return false, "the field is not one the person was asked about"
	}
	return true, ""
}

// fieldUnit is a field_unit row's text: the field (its label, on a form of an application) and whether the person said
// it takes its figures in the unit.
type fieldUnit struct {
	Label string `json:"label"`
	Host  string `json:"host"`
	Form  string `json:"form"`
	Unit  string `json:"unit"`
	Takes *bool  `json:"takes"`
}

// fieldUnitSentence is the sentence of the save's unit question whose option the person's reply is; the agent writes
// the same words (agent/decurix_agent/unit_answers.py sentence), spacing collapsed.
func fieldUnitSentence(label, host, form, unit string) string {
	return `Reply 1 if the field "` + strings.Join(strings.Fields(label), " ") + `" on ` + host + form +
		` takes its figures in ` + unit + `, or 2 if it does not.`
}

// checkFieldUnit reports whether text is a field_unit that stands on what the person was asked and said: the question
// they answered (asked) holds that field's unit sentence, and their reply (source) is the number of the option the
// row says ("1": it takes the unit, "2": it does not). Anything else is refused: a row never says more than the reply.
func checkFieldUnit(text, source, asked string) (ok bool, why string) {
	var fu fieldUnit
	if err := json.Unmarshal([]byte(text), &fu); err != nil || fu.Takes == nil || strings.TrimSpace(fu.Label) == "" ||
		strings.TrimSpace(fu.Host) == "" || strings.ContainsAny(fu.Host, " /") || strings.TrimSpace(fu.Unit) == "" ||
		strings.ContainsAny(fu.Unit, " \t\n") || len(fu.Label) > 120 || len(fu.Host) > 255 || len(fu.Form) > 500 ||
		len(fu.Unit) > 16 || (fu.Form != "" && !strings.HasPrefix(fu.Form, "/")) || strings.ContainsAny(fu.Form, " ") {
		return false, "a field_unit's text is {\"label\", \"host\", \"form\", \"unit\", \"takes\"}"
	}
	reply := strings.Trim(strings.Join(strings.Fields(source), " "), " .")
	if reply != "1" && reply != "2" {
		return false, "what the person said is not an option's number"
	}
	if *fu.Takes != (reply == "1") {
		return false, "the person's reply is not the option the row says"
	}
	if !strings.Contains(strings.Join(strings.Fields(asked), " "), fieldUnitSentence(fu.Label, fu.Host, fu.Form, fu.Unit)) {
		return false, "the field is not one the person was asked about"
	}
	return true, ""
}
