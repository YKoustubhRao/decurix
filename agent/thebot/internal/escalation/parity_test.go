package escalation

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// corpus is testdata/parity.json, recorded from the Python code (agent/tests/test_escalation_parity.py --write): for
// each case the evidence the ladder hands soul and the messages today's path sends, and for each answer the decision
// today's decide_* makes of it; for each awkward answer, every question's decision of it.
type corpus struct {
	Systems map[string]string `json:"systems"`
	Cases   []struct {
		Name     string          `json:"name"`
		Kind     string          `json:"kind"`
		Evidence json.RawMessage `json:"evidence"`
		Messages []Message       `json:"messages"`
		Answers  []struct {
			Text     string          `json:"text"`
			Decision json.RawMessage `json:"decision"`
		} `json:"answers"`
	} `json:"cases"`
	Answers []struct {
		Name      string                     `json:"name"`
		Text      string                     `json:"text"`
		Decisions map[string]json.RawMessage `json:"decisions"`
	} `json:"answers"`
}

func loadCorpus(t *testing.T) corpus {
	t.Helper()
	raw, err := os.ReadFile("testdata/parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var c corpus
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Cases) < 12 || len(c.Answers) < 60 {
		t.Fatalf("the corpus has %d cases and %d answers", len(c.Cases), len(c.Answers))
	}
	return c
}

// generic is a JSON text as plain values, so two encodings of the same decision compare equal.
func generic(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	return v
}

func TestTheSystemTextsArePythons(t *testing.T) {
	c := loadCorpus(t)
	for kind, want := range map[string]string{Ground: GroundSystem, Judge: JudgeSystem, Detour: DetourSystem, Point: PointSystem} {
		if c.Systems[kind] != want {
			t.Errorf("%s's system text differs from Python's:\n%q\n%q", kind, want, c.Systems[kind])
		}
	}
	if len(c.Systems) != 4 {
		t.Errorf("Python has %d questions", len(c.Systems))
	}
}

// TestSoulsMessagesArePythons: from the evidence Python hands over, soul builds exactly the messages today's path
// sends, and reads every recorded answer to the decision today's decide_* made of it.
func TestSoulsMessagesArePythons(t *testing.T) {
	for _, c := range loadCorpus(t).Cases {
		t.Run(c.Name, func(t *testing.T) {
			q, err := Prepare(c.Kind, c.Evidence)
			if err != nil {
				t.Fatal(err)
			}
			if got := q.Messages(); !reflect.DeepEqual(got, c.Messages) {
				t.Fatalf("messages differ:\n soul:   %q\n python: %q", got[1].Content, c.Messages[1].Content)
			}
			for i, a := range c.Answers {
				got, _ := json.Marshal(q.Decide(a.Text))
				if g, w := generic(t, got), generic(t, a.Decision); !reflect.DeepEqual(g, w) {
					t.Errorf("answer %d (%.60q): soul %s, python %s", i, a.Text, got, a.Decision)
				}
			}
		})
	}
}

// TestSoulDecidesAsPythonDoes: every awkward answer is the same decision here as in Python, for each question.
func TestSoulDecidesAsPythonDoes(t *testing.T) {
	c := loadCorpus(t)
	base := map[string]Question{}
	for _, k := range c.Cases {
		if _, ok := base[k.Kind]; !ok {
			q, err := Prepare(k.Kind, k.Evidence)
			if err != nil {
				t.Fatal(err)
			}
			base[k.Kind] = q
		}
	}
	for _, a := range c.Answers {
		t.Run(a.Name, func(t *testing.T) {
			for _, kind := range []string{Ground, Judge, Detour, Point} {
				got, _ := json.Marshal(base[kind].Decide(a.Text))
				if g, w := generic(t, got), generic(t, a.Decisions[kind]); !reflect.DeepEqual(g, w) {
					t.Errorf("%s: soul %s, python %s", kind, got, a.Decisions[kind])
				}
			}
		})
	}
}

// TestOnlyTheLaddersQuestionsAndTheirEvidence: a purpose that is not one of the four, or evidence of another shape (a
// field this side does not know, not an object), is refused.
func TestOnlyTheLaddersQuestionsAndTheirEvidence(t *testing.T) {
	for purpose, want := range map[string]string{"py.escalation.ground": Ground, "py.escalation.point": Point,
		"py.escalation.judge": Judge, "py.escalation.detour": Detour} {
		if kind, ok := KindOf(purpose); !ok || kind != want {
			t.Errorf("%s: %q %v", purpose, kind, ok)
		}
	}
	for _, purpose := range []string{"py.escalation", "py.escalation.", "py.escalation.ground.x", "py.judge", "escalation.ground", ""} {
		if _, ok := KindOf(purpose); ok {
			t.Errorf("%q was taken for a question of the ladder", purpose)
		}
	}
	for _, raw := range []string{`{"goal": "", "unknown": 1}`, `null`, `[]`, `"x"`, `{"candidates": "x"}`, `{} {}`} {
		if _, err := Prepare(Ground, json.RawMessage(raw)); err == nil {
			t.Errorf("%s was taken for a ground question's evidence", raw)
		}
	}
	if _, err := Prepare("other", json.RawMessage(`{}`)); err == nil {
		t.Error("an unknown question was prepared")
	}
}
