package judge

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// corpus is testdata/parity.json, recorded from the Python code (agent/tests/test_judge_parity.py --write): for each
// case the evidence the switch hands soul and the messages today's path sends, and for each answer the decision
// today's _parse makes of it.
type corpus struct {
	System string `json:"system"`
	Cases  []struct {
		Name     string            `json:"name"`
		Evidence Evidence          `json:"evidence"`
		Messages []json.RawMessage `json:"messages"`
		LookUp   *string           `json:"look_up"`
		Answers  []recorded        `json:"answers"`
	} `json:"cases"`
	Answers []struct {
		Name string `json:"name"`
		recorded
	} `json:"answers"`
}

type recorded struct {
	Text     string          `json:"text"`
	Decision json.RawMessage `json:"decision"`
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
	if len(c.Cases) < 10 || len(c.Answers) < 30 {
		t.Fatalf("the corpus has %d cases and %d answers", len(c.Cases), len(c.Answers))
	}
	return c
}

// generic is a JSON text as plain values, so two encodings of the same messages compare equal.
func generic(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	return v
}

func TestTheSystemPromptIsPythons(t *testing.T) {
	if c := loadCorpus(t); c.System != System {
		t.Fatalf("System differs from verify.JUDGE_SYSTEM:\n%q\n%q", System, c.System)
	}
}

// TestSoulsMessagesArePythons: from the evidence Python hands over, soul builds exactly the messages today's path sends
// - every text, every picture, in the same order - for the first judgement and for a closer look.
func TestSoulsMessagesArePythons(t *testing.T) {
	for _, c := range loadCorpus(t).Cases {
		t.Run(c.Name, func(t *testing.T) {
			for i, want := range c.Messages {
				var look []string
				closer := ""
				if i > 0 {
					var first Verdict
					if err := json.Unmarshal(c.Answers[0].Decision, &first); err != nil {
						t.Fatal(err)
					}
					look, closer = first.Look, *c.LookUp
				}
				got, err := json.Marshal(Messages(c.Evidence, look, closer))
				if err != nil {
					t.Fatal(err)
				}
				if g, w := generic(t, got), generic(t, want); !reflect.DeepEqual(g, w) {
					t.Fatalf("call %d differs:\n soul:   %.2000s\n python: %.2000s", i+1, got, want)
				}
			}
		})
	}
}

// TestSoulDecidesAsPythonDoes: every recorded answer is the same verdict here as in Python, and an answer Python could
// not read is not read here either, with the same message - except for JSON that does not decode, where the
// decoder's own words differ (the verdict, unsure, does not).
func TestSoulDecidesAsPythonDoes(t *testing.T) {
	c := loadCorpus(t)
	var all []struct {
		name string
		recorded
	}
	for _, k := range c.Cases {
		for i, a := range k.Answers {
			all = append(all, struct {
				name string
				recorded
			}{k.Name + "/" + string(rune('0'+i)), a})
		}
	}
	for _, a := range c.Answers {
		all = append(all, struct {
			name string
			recorded
		}{a.Name, a.recorded})
	}
	for _, a := range all {
		t.Run(a.name, func(t *testing.T) {
			got, err := json.Marshal(Decide(a.Text))
			if err != nil {
				t.Fatal(err)
			}
			want := generic(t, a.Decision).(map[string]any)
			if message, ok := want["error"].(string); ok && !strings.HasPrefix(message, "the check's answer was cut off") &&
				!strings.HasPrefix(message, "the check did not answer in JSON") {
				g := generic(t, got).(map[string]any)
				if g["error"] == nil || g["cut_off"] != false {
					t.Fatalf("JSON that does not decode: got %s, want an error that is no cut-off", got)
				}
				return
			}
			if g := generic(t, got); !reflect.DeepEqual(g, any(want)) {
				t.Fatalf("decision differs:\n soul:   %s\n python: %s", got, a.Decision)
			}
		})
	}
}
