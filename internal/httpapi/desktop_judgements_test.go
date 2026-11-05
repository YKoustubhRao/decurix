package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// getAsDesktop is a GET to handler as desktop from (requestDesktop), or from the host when from is "".
func getAsDesktop(s *Server, handler http.HandlerFunc, from, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if from != "" {
		req = req.WithContext(context.WithValue(req.Context(), callerKey{}, from))
	}
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

// personsAnswer has box ask a question on a new task and a person answer it (secret or not); it returns the task and
// the answer's judgement.
func personsAnswer(t *testing.T, s *Server, ask, answer string, secret bool) (string, string) {
	t.Helper()
	box := tasks.Worker{ID: "box", Name: "box"}
	route, _ := s.Tasks.Commit(tasks.NewRoute(), box, "fill in the vendor form", tasks.Origin{Kind: tasks.OriginPerson})
	_, _ = s.Tasks.RecordOutcome("box", route.TaskID, tasks.Outcome{Kind: tasks.OutcomeQuery, Reply: ask, Secret: secret})
	reply, _ := s.Tasks.Plan("box", answer)
	asha := tasks.Actor{ID: "u_asha", Name: "Asha", Roles: []string{"operator"}, Kind: tasks.ActorPerson}
	reply, _, err := s.Tasks.CommitReply(reply, box, answer, secret, tasks.ReplyBy{Actor: asha, Door: "chat"}, tasks.Origin{Kind: tasks.OriginPerson})
	if err != nil || reply.JudgementID == "" {
		t.Fatalf("reply = %+v, %v", reply, err)
	}
	return route.TaskID, reply.JudgementID
}

// A desktop reads the person's judgements on its own task - who said it, what was asked, the text and its hash -
// and never another desktop's, nor from the host; a secret one's text is never sent (H-PY-18).
func TestADesktopReadsThePersonsJudgementsOnItsOwnTasksOnly(t *testing.T) {
	s := chatServer(t)
	task, answer := personsAnswer(t, s, "Which payment terms?", "Net  30, as always", false)

	rec := getAsDesktop(s, s.desktopJudgementsHandler, "box", "/api/desktops/judgements?task="+task)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var got struct{ Judgements []desktopJudgement }
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	var j *desktopJudgement
	for i := range got.Judgements {
		if got.Judgements[i].ID == answer {
			j = &got.Judgements[i]
		}
	}
	if j == nil || j.Actor.Kind != tasks.ActorPerson || j.TaskID != task || j.DerivedFrom != "" || j.Secret ||
		j.Text != "Net  30, as always" || j.TextSHA256 != collapsedSHA256("Net 30, as always") || j.Asked != "Which payment terms?" {
		t.Fatalf("the answer was given as %+v (all: %s)", j, rec.Body)
	}

	other, _ := s.Tasks.Commit(tasks.NewRoute(), tasks.Worker{ID: "eve", Name: "eve"}, "eve's own", tasks.Origin{Kind: tasks.OriginPerson})
	for name, c := range map[string]struct {
		from, target string
		want         int
	}{
		"another desktop reading box's task": {"eve", "/api/desktops/judgements?task=" + task, http.StatusForbidden},
		"box reading eve's task":             {"box", "/api/desktops/judgements?task=" + other.TaskID, http.StatusForbidden},
		"the host":                           {"", "/api/desktops/judgements?task=" + task, http.StatusForbidden},
		"neither task nor skill":             {"box", "/api/desktops/judgements", http.StatusBadRequest},
		"both":                               {"box", "/api/desktops/judgements?task=" + task + "&skill=x", http.StatusBadRequest},
		"a task id of no shape":              {"box", "/api/desktops/judgements?task=../x", http.StatusBadRequest},
	} {
		if rec := getAsDesktop(s, s.desktopJudgementsHandler, c.from, c.target); rec.Code != c.want {
			t.Errorf("%s: %d %s, want %d", name, rec.Code, rec.Body, c.want)
		}
	}

	secretTask, secretAnswer := personsAnswer(t, s, "The portal PIN?", "Plum7Orbit", true)
	rec = getAsDesktop(s, s.desktopJudgementsHandler, "box", "/api/desktops/judgements?task="+secretTask)
	if strings.Contains(rec.Body.String(), "Plum7Orbit") || !strings.Contains(rec.Body.String(), secretAnswer) {
		t.Fatalf("a secret answer: %s", rec.Body)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	for _, j := range got.Judgements {
		if j.ID == secretAnswer && (!j.Secret || j.Text != "" || j.TextSHA256 != "" || j.Asked != "") {
			t.Fatalf("a secret answer was given as %+v", j)
		}
	}
}

// A desktop keeps a field_answer only from a person's own judgement on its own task whose text names the option, reads
// back its own rows with the judgement each came from, and a skill's taught words; another desktop sees none of them,
// and reading writes nothing (H-PY-18, rule 4).
func TestAFieldAnswerStandsOnWhatThePersonSaid(t *testing.T) {
	s := chatServer(t)
	task, answer := personsAnswer(t, s, "Which payment terms?", "Net 30, as always", false)
	field := func(option string) map[string]any {
		text, _ := json.Marshal(fieldAnswer{Label: "Payment terms", Host: "erp.example.test", Option: option})
		return map[string]any{"task_id": task, "kind": "field_answer", "skill": "vendor-form", "derived_from": answer, "text": string(text)}
	}
	rec := postLearned(t, s, "box", field("Net  30"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("a field_answer the person named: %d %s", rec.Code, rec.Body)
	}
	var created struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	for name, c := range map[string]struct {
		from string
		body map[string]any
		want int
	}{
		"an option the person did not say": {"box", field("Net 60"), http.StatusConflict},
		"a field the person was not asked about": {"box", func() map[string]any {
			b := field("Net 30")
			text, _ := json.Marshal(fieldAnswer{Label: "Delivery terms", Host: "erp.example.test", Option: "Net 30"})
			b["text"] = string(text)
			return b
		}(), http.StatusConflict},
		"a text that is no field":   {"box", map[string]any{"task_id": task, "kind": "field_answer", "derived_from": answer, "text": "Net 30"}, http.StatusConflict},
		"another desktop's posting": {"eve", field("Net 30"), http.StatusForbidden},
	} {
		if rec := postLearned(t, s, c.from, c.body); rec.Code != c.want {
			t.Errorf("%s: %d %s, want %d", name, rec.Code, rec.Body, c.want)
		}
	}
	secretTask, secretAnswer := personsAnswer(t, s, "The PIN?", "4821", true)
	text, _ := json.Marshal(fieldAnswer{Label: "PIN", Option: "4821"})
	if rec := postLearned(t, s, "box", map[string]any{"task_id": secretTask, "kind": "field_answer", "derived_from": secretAnswer, "text": string(text)}); rec.Code != http.StatusConflict {
		t.Errorf("a field_answer from a secret answer: %d %s", rec.Code, rec.Body)
	}

	before, _ := s.Tasks.JudgementsForWorker("box")
	rec = getAsDesktop(s, s.desktopLearnedHandler, "box", "/api/desktops/learned?kind=field_answer")
	var learned struct{ Learned []desktopLearnedRow }
	_ = json.Unmarshal(rec.Body.Bytes(), &learned)
	if rec.Code != http.StatusOK || len(learned.Learned) != 1 || learned.Learned[0].ID != created.ID || learned.Learned[0].DerivedFrom != answer ||
		learned.Learned[0].Source.Actor.Kind != tasks.ActorPerson || learned.Learned[0].Source.Text != "Net 30, as always" ||
		learned.Learned[0].Source.Asked != "Which payment terms?" ||
		!strings.Contains(learned.Learned[0].Text, `"option":"Net  30"`) {
		t.Fatalf("read back: %d %s", rec.Code, rec.Body)
	}
	if rec := getAsDesktop(s, s.desktopLearnedHandler, "eve", "/api/desktops/learned?kind=field_answer"); !strings.Contains(rec.Body.String(), `"learned":[]`) {
		t.Fatalf("another desktop read box's rows: %s", rec.Body)
	}
	if rec := getAsDesktop(s, s.desktopLearnedHandler, "box", "/api/desktops/learned?kind=lesson"); rec.Code != http.StatusBadRequest {
		t.Fatalf("another kind: %d", rec.Code)
	}
	if rec := getAsDesktop(s, s.desktopLearnedHandler, "", "/api/desktops/learned?kind=field_answer"); rec.Code != http.StatusForbidden {
		t.Fatalf("from the host: %d", rec.Code)
	}

	// The skill's taught words: every judgement on box's tasks that name it, the person's answer among them.
	rec = getAsDesktop(s, s.desktopJudgementsHandler, "box", "/api/desktops/judgements?skill=vendor-form")
	if !strings.Contains(rec.Body.String(), answer) || strings.Contains(rec.Body.String(), secretAnswer) {
		t.Fatalf("the skill's judgements: %s", rec.Body)
	}
	if rec := getAsDesktop(s, s.desktopJudgementsHandler, "eve", "/api/desktops/judgements?skill=vendor-form"); !strings.Contains(rec.Body.String(), `"judgements":[]`) {
		t.Fatalf("another desktop read box's skill: %s", rec.Body)
	}
	if after, _ := s.Tasks.JudgementsForWorker("box"); len(after) != len(before) {
		t.Fatalf("reading wrote rows: %d -> %d", len(before), len(after))
	}
}

// The doors are a desktop's on the primary listener only: the judgements read is a read-only container route, and
// neither is on the gated listener's mux.
func TestTheDesktopsJudgementDoorsArePrimaryOnly(t *testing.T) {
	if !containerMayCall(http.MethodGet, "/api/desktops/judgements") || containerMayCall(http.MethodPost, "/api/desktops/judgements") {
		t.Error("the judgements read is not a read-only desktop route")
	}
	s := chatServer(t)
	gated := http.NewServeMux()
	s.RegisterRoutes(gated)
	for _, path := range []string{"/api/desktops/judgements?task=t1", "/api/desktops/learned?kind=field_answer"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req = req.WithContext(context.WithValue(req.Context(), callerKey{}, "box"))
		rec := httptest.NewRecorder()
		gated.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "judgements") || strings.Contains(rec.Body.String(), `"learned"`) {
			t.Errorf("%s is served on the gated listener: %d %s", path, rec.Code, rec.Body)
		}
	}
}

// A label's words stand in the question as the agent reads them: case, punctuation, filler and synonyms aside, each a
// whole word (H-PY-19).
func TestAFieldAnswersLabelMustBeInTheQuestion(t *testing.T) {
	for _, c := range []struct {
		label, asked string
		want         bool
	}{
		{"Payment terms", "Which payment terms?", true},
		{"Payment Terms:", "What are the PAYMENT terms for this vendor?", true},
		{"Invoice No.", "Which invoice number goes here?", true},
		{"Amt", "What amount?", true},
		{"Terms", "Which payment terms?", true},
		{"Delivery terms", "Which payment terms?", false},
		{"Term", "Which payment terms?", false},
		{"Due date", "Which date?", false},
		{"", "Which payment terms?", false},
		{"Payment terms", "", false},
	} {
		if got := labelInQuestion(c.label, c.asked); got != c.want {
			t.Errorf("labelInQuestion(%q, %q) = %v, want %v", c.label, c.asked, got, c.want)
		}
	}
}
