// Package evals measures models on checked cases, field by field, so a
// specialist takes work from the general brain - or from the remote frontier
// brain - only where it has been shown to do better on this machine. Published
// benchmark numbers are where the catalog starts; these results replace them.
//
// A case is a request with a check that code can decide: a number, exact
// text, words that must appear, a pattern, or a multiple-choice letter. Cases
// live outside the repo (the test data folder), never in code.
//
// Depends on nothing internal.
package evals

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Case is one checked request.
type Case struct {
	ID        string `json:"id"`
	Domain    string `json:"domain"`
	Prompt    string `json:"prompt"`
	System    string `json:"system,omitempty"`
	Check     Check  `json:"check"`
	MaxTokens int    `json:"max_tokens,omitempty"`
}

// Check says how an answer is judged.
type Check struct {
	// Kind is number | exact | contains | regex | choice.
	Kind      string   `json:"kind"`
	Value     string   `json:"value,omitempty"`
	Values    []string `json:"values,omitempty"`
	Tolerance float64  `json:"tolerance,omitempty"` // for number: relative, default 1e-6
}

// ReadCases reads a JSON-lines file of cases, checking each is well formed.
func ReadCases(path string) ([]Case, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Case
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "//") {
			continue
		}
		var c Case
		if err := json.Unmarshal([]byte(text), &c); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		if c.Prompt == "" || c.Domain == "" {
			return nil, fmt.Errorf("%s:%d: a case needs a domain and a prompt", path, line)
		}
		switch c.Check.Kind {
		case "number", "exact", "regex", "choice":
			if c.Check.Value == "" {
				return nil, fmt.Errorf("%s:%d: check %s needs a value", path, line, c.Check.Kind)
			}
		case "contains":
			if len(c.Check.Values) == 0 && c.Check.Value == "" {
				return nil, fmt.Errorf("%s:%d: check contains needs values", path, line)
			}
		default:
			return nil, fmt.Errorf("%s:%d: check kind %q is not number, exact, contains, regex or choice", path, line, c.Check.Kind)
		}
		if c.ID == "" {
			c.ID = fmt.Sprintf("line-%d", line)
		}
		out = append(out, c)
	}
	return out, sc.Err()
}

var (
	thinkRe  = regexp.MustCompile(`(?s)<think>.*?</think>`)
	boxedRe  = regexp.MustCompile(`\boxed\{([^{}]*(?:\{[^{}]*\}[^{}]*)*)\}`)
	markRe   = regexp.MustCompile(`(?i)\b(?:final answer|the answer is|answer)\b\s*[:=]?\s*\**\s*`)
	numRe    = regexp.MustCompile(`-?\d[\d,]*(?:\.\d+)?(?:[eE][-+]?\d+)?`)
	choiceRe = regexp.MustCompile(`(?:(?i:answer(?: is)?|option|choice)\s*[:=]?\s*\(?\**([A-J])\b)|(?m:^\s*\(?([A-J])\)?\s*[.)]?\s*$)|\boxed\{([A-J])\}`)
)

// finalPart is the one place an answer states its result: the last
// \boxed{}; else the rest of the line after the last "answer is" (or
// "answer:", "final answer"); else the last line that says anything.
// Thinking is removed first. Only this part is judged: a check never hunts
// the whole text for a value that happens to appear in it.
func finalPart(answer string) string {
	a := thinkRe.ReplaceAllString(answer, "")
	if m := boxedRe.FindAllStringSubmatch(a, -1); len(m) > 0 {
		return m[len(m)-1][1]
	}
	if locs := markRe.FindAllStringIndex(a, -1); len(locs) > 0 {
		line, _, _ := strings.Cut(a[locs[len(locs)-1][1]:], "\n")
		return line
	}
	return lastLine(a)
}

// choicePart is where a multiple-choice answer states its letter: the last
// \boxed{}; else everything from the last "answer" to the end; else the
// last line that says anything.
func choicePart(answer string) string {
	a := thinkRe.ReplaceAllString(answer, "")
	if m := boxedRe.FindAllStringSubmatch(a, -1); len(m) > 0 {
		return `\boxed{` + m[len(m)-1][1] + `}`
	}
	if locs := markRe.FindAllStringIndex(a, -1); len(locs) > 0 {
		return a[locs[len(locs)-1][0]:]
	}
	return lastLine(a)
}

// lastLine is the last line of a text that holds anything.
func lastLine(a string) string {
	lines := strings.Split(strings.TrimSpace(a), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return lines[i]
		}
	}
	return ""
}

// Decides reports whether a check is decided in code by one stated value
// (a number, a letter, exact text). Only such cases may move work to a
// cheaper path (Results.Proven): "contains" and "regex" find words anywhere
// and cannot tell an answer from a sentence that mentions it.
func (c Check) Decides() bool {
	switch c.Kind {
	case "number", "choice", "exact":
		return true
	}
	return false
}

// Correct judges one answer. A number or a letter must be stated once, in
// the answer's final part: two different values there ("13, not 12") is a
// miss, whichever one is right.
func (c Check) Correct(answer string) bool {
	switch c.Kind {
	case "number":
		want, err := parseNum(c.Value)
		if err != nil {
			return false
		}
		tol := c.Tolerance
		if tol <= 0 {
			tol = 1e-6
		}
		same := func(a, b float64) bool { return math.Abs(a-b) <= tol*math.Max(1, math.Abs(b)) }
		nums := numRe.FindAllString(finalPart(answer), -1)
		if len(nums) == 0 {
			return false
		}
		got, err := parseNum(nums[0])
		if err != nil {
			return false
		}
		for _, n := range nums[1:] {
			if v, err := parseNum(n); err != nil || !same(v, got) {
				return false
			}
		}
		return same(got, want)
	case "exact":
		return norm(finalPart(answer)) == norm(c.Value) || norm(answer) == norm(c.Value)
	case "contains":
		vals := c.Values
		if len(vals) == 0 {
			vals = []string{c.Value}
		}
		low := strings.ToLower(thinkRe.ReplaceAllString(answer, ""))
		for _, v := range vals {
			if !strings.Contains(low, strings.ToLower(v)) {
				return false
			}
		}
		return true
	case "regex":
		re, err := regexp.Compile(c.Value)
		return err == nil && re.MatchString(thinkRe.ReplaceAllString(answer, ""))
	case "choice":
		letters := map[string]bool{}
		var got string
		for _, m := range choiceRe.FindAllStringSubmatch(choicePart(answer), -1) {
			for _, g := range m[1:] {
				if g != "" {
					letters[g], got = true, g
				}
			}
		}
		return len(letters) == 1 && got == strings.ToUpper(strings.TrimSpace(c.Value))
	}
	return false
}

func parseNum(s string) (float64, error) {
	return strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(s), ",", ""), 64)
}

func norm(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.Trim(s, ".!*` \"'")
	return strings.Join(strings.Fields(s), " ")
}

// Score is one model's result in one field.
type Score struct {
	N       int   `json:"n"`
	Correct int   `json:"correct"`
	MeanMs  int64 `json:"mean_ms"`
	// MeanPrompt and MeanCompletion are the tokens one case used, which is
	// what a hosted model bills.
	MeanPrompt     int `json:"mean_prompt,omitempty"`
	MeanCompletion int `json:"mean_completion,omitempty"`
	// At is when it was measured; a live miss after this undoes what it earned.
	At time.Time `json:"at,omitempty"`
	// Decided and DecidedCorrect count the cases whose check is decided in
	// code by one stated value (Check.Decides). Only these prove anything.
	Decided        int `json:"decided,omitempty"`
	DecidedCorrect int `json:"decided_correct,omitempty"`
}

// decided is the part of a score that can prove a cheaper path.
func (s Score) decided() Score { return Score{N: s.Decided, Correct: s.DecidedCorrect} }

// Accuracy is correct over n.
func (s Score) Accuracy() float64 {
	if s.N == 0 {
		return 0
	}
	return float64(s.Correct) / float64(s.N)
}

// Results are every model's scores by field, kept in <home>/evals.json.
type Results struct {
	Updated time.Time                   `json:"updated"`
	Scores  map[string]map[string]Score `json:"scores"` // model -> domain -> score
}

// MinCases is how many checked cases in a field a comparison needs before it
// may move work: fewer is an anecdote, not a measurement.
const MinCases = 20

// Compare reports whether model a did at least as well as b in a field, and
// whether both have enough cases to say. Ties go to a: a specialist that does
// as well as the frontier on a field may take it.
func (r *Results) Compare(a, b, domain string) (aAtLeastB, known bool) {
	if r == nil {
		return false, false
	}
	sa, oka := r.Scores[a][domain]
	sb, okb := r.Scores[b][domain]
	if !oka || !okb || sa.N < MinCases || sb.N < MinCases {
		return false, false
	}
	return sa.Accuracy() >= sb.Accuracy(), true
}

// Margin is how much accuracy a cheaper path may give up: none. A path that
// is measurably worse never takes work to save tokens. z is the one-sided
// 95% confidence behind LowerBound.
const (
	Margin = 0.0
	z      = 1.645
)

// LowerBound is the accuracy a score can honestly claim: the Wilson lower
// bound, which stays well below correct/n when n is small.
func (s Score) LowerBound() float64 {
	if s.N == 0 {
		return 0
	}
	n, p := float64(s.N), s.Accuracy()
	mid := p + z*z/(2*n)
	spread := z * math.Sqrt(p*(1-p)/n+z*z/(4*n*n))
	return (mid - spread) / (1 + z*z/n)
}

// Proven reports whether checked cases show a is not worse than b in a field:
// the accuracy a can claim (its Wilson lower bound) is at least what b
// scored. Compare asks who did better; this asks whether the gap could be
// chance, so a small sample never moves work to a cheaper path. Nothing can
// prove itself as good as a perfect score; 20 of 20 can claim about 88%,
// enough to replace a model that scored 80% but not one that scored 90%. Only
// cases decided by one stated value count (Check.Decides).
func (r *Results) Proven(a, b, domain string) bool {
	if r == nil {
		return false
	}
	sa, oka := r.Scores[a][domain]
	sb, okb := r.Scores[b][domain]
	da, db := sa.decided(), sb.decided()
	if !oka || !okb || da.N < MinCases || db.N < MinCases {
		return false
	}
	return da.LowerBound() >= db.Accuracy()-Margin
}

// Set records a score, replacing the model's earlier one in that field.
func (r *Results) Set(model, domain string, s Score) {
	if r.Scores == nil {
		r.Scores = map[string]map[string]Score{}
	}
	if r.Scores[model] == nil {
		r.Scores[model] = map[string]Score{}
	}
	r.Scores[model][domain] = s
	r.Updated = time.Now().UTC()
}

// Load reads results from a file; a missing file is empty results.
func Load(path string) (*Results, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Results{Scores: map[string]map[string]Score{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var r Results
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if r.Scores == nil {
		r.Scores = map[string]map[string]Score{}
	}
	return &r, nil
}

// Save writes results to a file.
func (r *Results) Save(path string) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
