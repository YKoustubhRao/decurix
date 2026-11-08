package judge

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"

	"thebot/internal/pyread"
)

// The judge's verdicts (verify.py DONE, NOT_DONE, UNSURE, CANNOT), and the least confidence a done needs to stay done.
const (
	Done           = "done"
	NotDone        = "not_done"
	Unsure         = "unsure"
	Cannot         = "cannot"
	DoneConfidence = 0.8
)

// Verdict is the judge's answer as read (verify._parse's shape): Look is always a list, PutBack only when it names
// something.
type Verdict struct {
	Verdict    string   `json:"verdict"`
	Confidence float64  `json:"confidence"`
	Seen       string   `json:"seen"`
	Why        string   `json:"why"`
	Look       []string `json:"look"`
	PutBack    []string `json:"put_back,omitempty"`
}

// ParseError is an answer that could not be read: its message is verify._parse's (for an answer cut off, or with no
// JSON in it, word for word; for JSON that does not decode, the decoder's own words, which differ from Python's).
// CutOff marks an answer that stopped before its JSON ended (verify.CutOff).
type ParseError struct {
	Message string
	CutOff  bool
}

// Error is the message.
func (e *ParseError) Error() string { return e.Message }

// Decision is what soul answers for one judgement: the verdict, or the error that kept the answer from being read.
// The error's mapping to a verdict (unsure, "the check could not be made (...)") stays the caller's (verify.judge).
type Decision struct {
	Verdict *Verdict
	Err     *ParseError
}

// MarshalJSON writes the verdict, or {"error", "cut_off"}.
func (d Decision) MarshalJSON() ([]byte, error) {
	if d.Err != nil {
		return json.Marshal(map[string]any{"error": d.Err.Message, "cut_off": d.Err.CutOff})
	}
	return json.Marshal(d.Verdict)
}

// Decide reads the model's answer as verify.judge's ask() does: Parse, then a done less sure than DoneConfidence is
// unsure ("not sure enough"). Never a done that the answer did not give with confidence.
func Decide(text string) Decision {
	v, err := Parse(text)
	if err != nil {
		return Decision{Err: err}
	}
	if v.Verdict == Done && v.Confidence < DoneConfidence {
		v.Why = fmt.Sprintf("not sure enough (%.2f): %s", v.Confidence, v.Why)
		v.Verdict = Unsure
	}
	return Decision{Verdict: &v}
}

// object is the first "{" to the last "}" of an answer (verify._parse: re.search(r"\{.*\}", text, re.DOTALL)).
var object = regexp.MustCompile(`(?s)\{.*\}`)

// Parse reads the judge's answer as verify._parse does: the JSON object in it, its verdict (unknown is unsure), its
// confidence (0 to 1; one that is not a number is 0), seen and why (one line, at most 400 characters), look (at most
// MaxLook terms of 80) and put_back (at most MaxPutBack names of 300). Each field is read as Python reads it (str(),
// float(), split()), so the same answer is the same verdict on both sides.
func Parse(text string) (Verdict, *ParseError) {
	loc := object.FindStringIndex(text)
	if loc == nil {
		if strings.Contains(text, "{") {
			return Verdict{}, &ParseError{Message: "the check's answer was cut off before it ended: " + pyread.ReprString(pyread.Head(text, 200)), CutOff: true}
		}
		return Verdict{}, &ParseError{Message: "the check did not answer in JSON: " + pyread.ReprString(pyread.Head(text, 200))}
	}
	data, err := pyread.Decode(text[loc[0]:loc[1]])
	if err != nil {
		return Verdict{}, &ParseError{Message: err.Error()}
	}
	if data.Kind != 'o' {
		// Never: the text matched is "{...}", which decodes only as an object.
		return Verdict{}, &ParseError{Message: "the check's answer is not a JSON object"}
	}
	v := Verdict{Look: []string{}}
	given, ok := data.Get("verdict")
	v.Verdict = strings.ReplaceAll(strings.ToLower(pyread.Strip(pyread.StrOr(given, ok))), " ", "_")
	switch v.Verdict {
	case Done, NotDone, Unsure, Cannot:
	default:
		v.Verdict = Unsure
	}
	// float(data.Get("confidence", 0)), 0 when it is no number or no finite one ("nan", "inf": verify._parse reads
	// those as 0 too, never as a sure done), then clamped to 0..1.
	confidence := 0.0
	if c, ok := data.Get("confidence"); ok {
		if f, ok := pyread.Float(c); ok && !math.IsNaN(f) && !math.IsInf(f, 0) {
			confidence = f
		}
	}
	low := 1.0
	if confidence < low {
		low = confidence
	}
	v.Confidence = 0.0
	if low > v.Confidence {
		v.Confidence = low
	}
	seen, ok := data.Get("seen")
	v.Seen = pyread.Head(strings.Join(pyread.Split(pyread.StrOr(seen, ok)), " "), 400)
	why, ok := data.Get("why")
	v.Why = pyread.Head(strings.Join(pyread.Split(pyread.StrOr(why, ok)), " "), 400)
	if look, ok := data.Get("look"); ok && look.Kind == 'l' {
		for _, t := range look.Items {
			s := pyread.Str(t)
			if pyread.Strip(s) == "" {
				continue
			}
			if len(v.Look) == MaxLook {
				break
			}
			v.Look = append(v.Look, pyread.Head(strings.Join(pyread.Split(s), " "), 80))
		}
	}
	// The files whose change the request did not ask for: names only (strings), or nothing. Only the gate decides
	// what they point at (source_guard.named_in).
	if back, ok := data.Get("put_back"); ok && back.Kind == 'l' {
		for _, t := range back.Items {
			if t.Kind != 's' || pyread.Strip(t.Str) == "" {
				continue
			}
			if len(v.PutBack) == MaxPutBack {
				break
			}
			v.PutBack = append(v.PutBack, pyread.Head(strings.Join(pyread.Split(t.Str), " "), 300))
		}
	}
	return v, nil
}
