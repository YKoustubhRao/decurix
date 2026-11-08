// Package escalation is the escalation ladder's four questions to a model, made in soul (plan rule 3, phase 2, the
// second decision moved: py.escalation.*). When a taught skill's replay cannot find a control, or its recorded result
// is not there exactly, the ladder asks: which of the candidates on the screen is the recorded control (ground);
// whether the screen shows the step did its job (judge); whether the steps ahead dealt with something this run does not
// meet and may be left out (detour); which line of text on the screen now plays the part the recorded click did
// (point). The Python agent keeps the tools - it ranks the candidates, renders the screen, reads the text off the
// recording's frames - and hands them over as data (escalation.py's *_evidence); this package words them into the
// prompt and decides what the model's answer says.
//
// Today's path is Python's own (agent/decurix_agent/computer/escalation.py: GROUND_SYSTEM and the other three, the
// *_prompt builders, parse_reply and the decide_* readers), and while the switch (DECURIX_SOUL_ESCALATION) is off by
// default both exist: every text here is byte for byte Python's, and the reading of an answer follows Python's
// (json.loads, int(), float(), str(), its slices; internal/pyread). testdata/parity.json, recorded from the Python code,
// proves it (parity_test.go here, agent/tests/test_escalation_parity.py there); a change to either side changes both
// and regenerates the corpus.
package escalation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// The ladder's questions, each the last part of its purpose (py.escalation.<kind>).
const (
	Ground = "ground"
	Judge  = "judge"
	Detour = "detour"
	Point  = "point"
)

// purposePrefix is what every purpose of the ladder's begins with.
const purposePrefix = "py.escalation."

// KindOf is the question a purpose names: py.escalation.ground, .judge, .detour or .point, and nothing else.
func KindOf(purpose string) (string, bool) {
	kind, ok := strings.CutPrefix(purpose, purposePrefix)
	switch {
	case !ok:
		return "", false
	case kind == Ground, kind == Judge, kind == Detour, kind == Point:
		return kind, true
	}
	return "", false
}

// Message is one OpenAI-shaped message of a question: the system's text, then the prompt (harness.ask_model's shape).
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Question is one of the ladder's questions ready to ask: its system text, its prompt, and how its answer is read.
type Question struct {
	Kind   string
	System string
	Prompt string
	decide func(text string) any
}

// Messages is the question as the model is asked it: the system's text, then the prompt.
func (q Question) Messages() []Message {
	return []Message{{Role: "system", Content: q.System}, {Role: "user", Content: q.Prompt}}
}

// Decide is soul's reading of the model's answer to the question: a Pick (ground, point), a Judgement (judge) or a
// Skip (detour), the decision escalation.py's decide_* makes of the same text.
func (q Question) Decide(text string) any { return q.decide(text) }

// Prepare is the question kind asks of its evidence (the JSON escalation.py's *_evidence makes): an error for evidence
// of another shape - a field this side does not know is refused, so the two sides never drift apart in silence.
func Prepare(kind string, evidence json.RawMessage) (Question, error) {
	switch kind {
	case Ground:
		var ev GroundEvidence
		if err := strict(evidence, &ev); err != nil {
			return Question{}, err
		}
		return Question{Kind: kind, System: GroundSystem, Prompt: GroundPrompt(ev),
			decide: func(text string) any { return DecideGround(text, ev) }}, nil
	case Judge:
		var ev JudgeEvidence
		if err := strict(evidence, &ev); err != nil {
			return Question{}, err
		}
		return Question{Kind: kind, System: JudgeSystem, Prompt: JudgePrompt(ev),
			decide: func(text string) any { return DecideJudge(text) }}, nil
	case Detour:
		var ev DetourEvidence
		if err := strict(evidence, &ev); err != nil {
			return Question{}, err
		}
		return Question{Kind: kind, System: DetourSystem, Prompt: DetourPrompt(ev),
			decide: func(text string) any { return DecideDetour(text) }}, nil
	case Point:
		var ev PointEvidence
		if err := strict(evidence, &ev); err != nil {
			return Question{}, err
		}
		return Question{Kind: kind, System: PointSystem, Prompt: PointPrompt(ev),
			decide: func(text string) any { return DecidePoint(text, ev) }}, nil
	}
	return Question{}, fmt.Errorf("%q is not one of the ladder's questions", kind)
}

// strict decodes one JSON object into v, refusing a field v does not have, null, and anything after it.
func strict(raw json.RawMessage, v any) error {
	if t := bytes.TrimSpace(raw); len(t) == 0 || t[0] != '{' {
		return fmt.Errorf("the evidence is not a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("the evidence is not of this question's shape: %w", err)
	}
	if dec.More() {
		return fmt.Errorf("the evidence is followed by more")
	}
	return nil
}
