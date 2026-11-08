// The ladder's readings of a model's answer: the same decision escalation.py's decide_* makes of the same text.

package escalation

import (
	"math"
	"regexp"

	"thebot/internal/pyread"
)

// The least confidence each answer needs to be acted on (escalation.py GROUND_CONFIDENCE, JUDGE_CONFIDENCE,
// DETOUR_CONFIDENCE); a point's line takes the grounding one.
const (
	GroundConfidence = 0.75
	JudgeConfidence  = 0.85
	DetourConfidence = 0.7
	// ReasonChars is how much of a detour's reason is kept for the run's trail.
	ReasonChars = 80
)

// What a Pick says (escalation.py CHOSEN, DOUBT, NONE): a choice taken; one in the list but not sure enough; none.
const (
	Chosen = "chosen"
	Doubt  = "doubt"
	None   = "none"
)

// What a Skip says (escalation.py SKIP, KEEP, UNSURE): leave the steps out; they are still needed; cannot tell.
const (
	SkipThem = "skip"
	KeepThem = "keep"
	Unsure   = "unsure"
)

// Pick is the answer to a choice among numbered things (ground, point): the choice (0 when it is none of them) and the
// confidence (0 to 1), and the verdict they make.
type Pick struct {
	Choice     int     `json:"choice"`
	Confidence float64 `json:"confidence"`
	Verdict    string  `json:"verdict"`
}

// Judgement is the answer to whether a step did its job: a yes given with enough confidence, and the confidence.
type Judgement struct {
	Achieved   bool    `json:"achieved"`
	Confidence float64 `json:"confidence"`
}

// Skip is the answer to whether steps may be left out: the verdict, the confidence and the reason given (str() of it,
// at most ReasonChars characters).
type Skip struct {
	Verdict    string  `json:"verdict"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
}

// object is a JSON object in a model's answer (escalation.JSON_OBJECT: r"\{.*?\}" with DOTALL): a "{" to the first "}".
var object = regexp.MustCompile(`(?s)\{.*?\}`)

// Reply is escalation.parse_reply: the first "{...}" in the text, in order, that json.loads reads (each tried as
// Python's finditer finds them), and whether there was one. Models wrap it in prose and fences.
func Reply(text string) (pyread.Value, bool) {
	for _, loc := range object.FindAllStringIndex(text, -1) {
		if v, err := pyread.Loads(text[loc[0]:loc[1]]); err == nil && v.Kind == 'o' {
			return v, true
		}
	}
	return pyread.Value{Kind: 'o'}, false
}

// confidence is escalation._confidence: float() of the value, clamped to 0..1; 0 for no number or no finite one.
func confidence(v pyread.Value, present bool) float64 {
	if !present {
		return 0
	}
	f, ok := pyread.Float(v)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return math.Max(0, math.Min(1, f))
}

// choice is escalation._choice: int(answer.get("choice") or 0), 0 for anything int() refuses.
func choice(answer pyread.Value) int64 {
	v, ok := answer.Get("choice")
	if !ok || !v.Truthy() {
		return 0
	}
	n, ok := pyread.Int(v)
	if !ok {
		return 0
	}
	return n
}

// pick is escalation._pick: the choice among count things and its confidence, read from the answer, and their verdict
// against the least confidence that is enough.
func pick(text string, count int, enough float64) Pick {
	answer, _ := Reply(text)
	n := choice(answer)
	c, ok := answer.Get("confidence")
	p := Pick{Confidence: confidence(c, ok), Verdict: None}
	if n < 1 || n > int64(count) {
		return p
	}
	p.Choice = int(n)
	p.Verdict = Doubt
	if p.Confidence >= enough {
		p.Verdict = Chosen
	}
	return p
}

// DecideGround reads the grounding question's answer (escalation.decide_ground): a choice among the candidates.
func DecideGround(text string, ev GroundEvidence) Pick {
	return pick(text, len(ev.Candidates), GroundConfidence)
}

// DecidePoint reads the answer of where a point click goes (escalation.decide_point): a choice among the lines.
func DecidePoint(text string, ev PointEvidence) Pick {
	return pick(text, len(ev.Lines), GroundConfidence)
}

// DecideJudge reads the judging question's answer (escalation.decide_judge): achieved only for a JSON true given with
// at least JudgeConfidence.
func DecideJudge(text string) Judgement {
	answer, _ := Reply(text)
	c, ok := answer.Get("confidence")
	j := Judgement{Confidence: confidence(c, ok)}
	achieved, ok := answer.Get("achieved")
	j.Achieved = ok && achieved.Kind == 'b' && achieved.Bool && j.Confidence >= JudgeConfidence
	return j
}

// DecideDetour reads the detour question's answer (escalation.decide_detour): skip for a JSON true, keep for a JSON
// false, each given with at least DetourConfidence; unsure otherwise.
func DecideDetour(text string) Skip {
	answer, _ := Reply(text)
	c, ok := answer.Get("confidence")
	reason, has := answer.Get("reason")
	s := Skip{Verdict: Unsure, Confidence: confidence(c, ok), Reason: pyread.Head(pyread.StrOr(reason, has), ReasonChars)}
	if skip, ok := answer.Get("skip"); ok && skip.Kind == 'b' && s.Confidence >= DetourConfidence {
		s.Verdict = KeepThem
		if skip.Bool {
			s.Verdict = SkipThem
		}
	}
	return s
}
