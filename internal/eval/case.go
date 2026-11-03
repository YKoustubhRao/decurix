// Package eval runs declarative suites of cases against workers in isolated practice desktops and grades every case
// in code (docs/evals.md). A suite arrives as a tar; this package never reads a suite from disk, so the control plane
// can run in a container, and harness code never names a suite.
package eval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Schema names of a suite's two documents.
const (
	SuiteSchema = "decurix.eval-suite/v1"
	CaseSchema  = "decurix.eval-case/v1"
)

// Outcomes a case can expect, and the observed outcomes besides them.
const (
	OutcomeComplete = "complete"
	OutcomeAsked    = "asked"
	OutcomeApproval = "approval"
	OutcomeError    = "error"
	OutcomeTimeout  = "timeout"
	OutcomeBudget   = "budget"
)

// Trust levels a grader reports (D8). Independent, output and model checks decide a pass; a worker-written one never
// does. A model check is one a model's judgement settles (which fact a question is about, read by the scripted
// expert): the report says so beside it.
const (
	TrustIndependent   = "independent"
	TrustOutput        = "output"
	TrustModel         = "model"
	TrustWorkerWritten = "worker_written"
)

// Results of one check.
const (
	CheckPass    = "pass"
	CheckFail    = "fail"
	CheckNotMade = "not_made"
)

// Limits of a suite.
const (
	MaxSuiteBytes   = 64 << 20
	MaxCases        = 200
	MaxGraders      = 30
	MaxFacts        = 20
	MaxOracleSteps  = 200
	MaxRequestBytes = 8000
	MaxCaseMinutes  = 60
)

// Suite is a loaded, validated suite: its own document plus every case, file and mock it names.
type Suite struct {
	Schema      string              `json:"schema"`
	Name        string              `json:"name"`
	Description string              `json:"description"`
	Mocks       map[string]MockSpec `json:"mocks"`
	Budget      Budget              `json:"budget"`
	CaseIDs     []string            `json:"cases"`

	// Filled by Load.
	Cases       []Case            `json:"-"`
	MockSources map[string][]byte `json:"-"`
	SHA         string            `json:"-"`
}

// MockSpec is one mock app a suite uses, and the web hosts the practice browser maps to it.
type MockSpec struct {
	Hosts []string `json:"hosts"`
}

// Budget bounds one case: tokens across its model calls, and minutes of wall time.
type Budget struct {
	MaxTokens  int64   `json:"max_tokens,omitempty"`
	MaxMinutes float64 `json:"max_minutes,omitempty"`
}

// Case is one request, its start state, the facts the person knows, what must be true at the end, and a scripted
// solution that proves it can be done.
type Case struct {
	Schema        string       `json:"schema"`
	ID            string       `json:"id"`
	Category      string       `json:"category"`
	Kind          string       `json:"kind"`
	Request       string       `json:"request"`
	Start         Start        `json:"start"`
	Facts         []Fact       `json:"facts"`
	Expected      Expected     `json:"expected"`
	Oracle        []OracleStep `json:"oracle"`
	OracleSecrets []string     `json:"oracle_secrets,omitempty"`
	Budget        Budget       `json:"budget"`
	Tags          []string     `json:"tags,omitempty"`

	// Filled by Load: the input files (by flat name) and the mock's start state.
	Files   map[string][]byte `json:"-"`
	Fixture []byte            `json:"-"`
}

// Start is the case's start state: files copied into its input folder, and the mock it runs against.
type Start struct {
	Files   []string `json:"files"`
	Mock    string   `json:"mock,omitempty"`
	Fixture string   `json:"fixture,omitempty"`
}

// Fact is something the person who gave the task knows. The expert chooses one by id; the worker is sent Answer.
type Fact struct {
	ID       string `json:"id"`
	About    string `json:"about"`
	Answer   string `json:"answer"`
	Required *bool  `json:"required"`
}

// IsRequired reports the fact's required flag (false when unset; Load refuses an unset one).
func (f Fact) IsRequired() bool { return f.Required != nil && *f.Required }

// Expected is the outcome a case must end in and the graders that must pass.
type Expected struct {
	Outcome string `json:"outcome"`
	// Accept lists other outcomes that also pass a complete case (only "asked" for now): a worker that asks is not
	// wrong where asking is a right answer. Under one, only the graders scoped to it count.
	Accept  []string `json:"accept,omitempty"`
	Graders []Grader `json:"graders"`
	// OthersMayChange lets the mock's collections no grader names change. By default they must stay as the fixture
	// left them: a case that adds a user must not also delete one.
	OthersMayChange bool `json:"others_may_change,omitempty"`
}

// Grader is one check of a case's evidence. Kind decides which fields it reads (docs/evals.md).
type Grader struct {
	Kind string `json:"kind"`
	// ID names the check in reports; the kind and position when empty.
	ID string `json:"id,omitempty"`
	// When scopes the grader to one outcome; empty takes the kind's own scope.
	When string `json:"when,omitempty"`

	Collection string `json:"collection,omitempty"`
	ExactCount bool   `json:"exact_count,omitempty"`
	// Records is a list of expected records (mock_records, absent) or a count (ledger_readback).
	Records json.RawMessage `json:"records,omitempty"`
	// Written is how the app writes a digit date that is not ISO: "dmy" or "mdy".
	Written string `json:"written,omitempty"`

	File  string          `json:"file,omitempty"`
	Text  string          `json:"text,omitempty"`
	JSON  json.RawMessage `json:"json,omitempty"`
	Rows  [][]string      `json:"rows,omitempty"`
	Cells []CellCheck     `json:"cells,omitempty"`

	Words  []string `json:"words,omitempty"`
	Any    []string `json:"any,omitempty"`
	Number string   `json:"number,omitempty"`

	To          []string `json:"to,omitempty"`
	Cc          []string `json:"cc,omitempty"`
	Attachments []string `json:"attachments,omitempty"`
	Contains    []string `json:"contains,omitempty"`
	Drafts      bool     `json:"drafts,omitempty"`

	Facts          []string `json:"facts,omitempty"`
	Mentions       []string `json:"mentions,omitempty"`
	ActionContains string   `json:"action_contains,omitempty"`
}

// CellCheck is readiness's workbook cell check (qualification.CellCheck), as a case writes it.
type CellCheck struct {
	Sheet       string `json:"sheet"`
	Cell        string `json:"cell"`
	Value       string `json:"value"`
	Formula     *bool  `json:"formula,omitempty"`
	FormulaText string `json:"formula_text,omitempty"`
}

// graderKind is what the library knows of one kind: its trust and the outcome it is scoped to by default.
type graderKind struct {
	trust string
	scope string // "" counts under every outcome
}

// graderKinds is the grader library (EVAL-2). A kind not listed here is refused at load.
var graderKinds = map[string]graderKind{
	"mock_records":        {TrustIndependent, OutcomeComplete},
	"absent":              {TrustIndependent, ""},
	"file_text_equals":    {TrustIndependent, OutcomeComplete},
	"file_text_contains":  {TrustIndependent, OutcomeComplete},
	"json_equals":         {TrustIndependent, OutcomeComplete},
	"csv_equals":          {TrustIndependent, OutcomeComplete},
	"file_cells":          {TrustIndependent, OutcomeComplete},
	"outbox":              {TrustIndependent, OutcomeComplete},
	"final_text_number":   {TrustOutput, OutcomeComplete},
	"final_text_contains": {TrustOutput, OutcomeComplete},
	"asked":               {TrustModel, OutcomeAsked},
	"stopped_at_approval": {TrustIndependent, OutcomeApproval},
	"ledger_readback":     {TrustWorkerWritten, OutcomeComplete},
}

// GraderKinds lists the library's kinds, for docs and tests.
func GraderKinds() []string {
	out := make([]string, 0, len(graderKinds))
	for k := range graderKinds {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

// Trust is the grader's trust level ("" for an unknown kind).
func (g Grader) Trust() string { return graderKinds[g.Kind].trust }

// Scope is the outcome the grader counts under ("" for every outcome).
func (g Grader) Scope() string {
	if g.When != "" {
		return g.When
	}
	return graderKinds[g.Kind].scope
}

// RecordList decodes Records as expected records.
func (g Grader) RecordList() ([]map[string]Value, error) {
	if len(bytes.TrimSpace(g.Records)) == 0 {
		return nil, nil
	}
	var out []map[string]Value
	d := json.NewDecoder(bytes.NewReader(g.Records))
	if err := d.Decode(&out); err != nil {
		return nil, fmt.Errorf("records must be a list of objects: %v", err)
	}
	return out, nil
}

// RecordCount decodes Records as a count (ledger_readback).
func (g Grader) RecordCount() (int, error) {
	var n int
	if err := json.Unmarshal(g.Records, &n); err != nil {
		return 0, fmt.Errorf("records must be a whole number")
	}
	return n, nil
}

// Value kinds of an expected field (EVAL-2's typed values).
const (
	ValuePlain  = ""
	ValueID     = "id"
	ValueNumber = "number"
	ValueDate   = "date"
	ValueText   = "text"
)

// Value is one expected field: a plain string (trimmed, exact), or {"id"|"number"|"date"|"text": s}.
type Value struct {
	Kind string
	S    string
}

// UnmarshalJSON reads a plain string, a JSON number or boolean (as written), or a one-key typed object.
func (v *Value) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return fmt.Errorf("empty value")
	}
	switch b[0] {
	case '"':
		v.Kind = ValuePlain
		return json.Unmarshal(b, &v.S)
	case '{':
		var m map[string]string
		if err := json.Unmarshal(b, &m); err != nil {
			return fmt.Errorf("a typed value is {\"id\"|\"number\"|\"date\"|\"text\": \"...\"}")
		}
		if len(m) != 1 {
			return fmt.Errorf("a typed value has exactly one key")
		}
		for k, s := range m {
			switch k {
			case ValueID, ValueNumber, ValueDate, ValueText:
				v.Kind, v.S = k, s
				return nil
			}
			return fmt.Errorf("unknown value type %q", k)
		}
	case 't', 'f':
		v.Kind = ValuePlain
		v.S = string(b)
		return nil
	default:
		var n json.Number
		if err := json.Unmarshal(b, &n); err != nil {
			return fmt.Errorf("a value is a string, a number or a typed object")
		}
		v.Kind, v.S = ValueNumber, n.String()
		return nil
	}
	return nil
}

// MarshalJSON writes the value back as a case writes it.
func (v Value) MarshalJSON() ([]byte, error) {
	if v.Kind == ValuePlain {
		return json.Marshal(v.S)
	}
	return json.Marshal(map[string]string{v.Kind: v.S})
}

// String is the value as a report shows it.
func (v Value) String() string {
	if v.Kind == ValuePlain {
		return v.S
	}
	return v.Kind + ":" + v.S
}

// Oracle step kinds.
const (
	StepRun      = "run"
	StepAnswer   = "answer"
	StepApproval = "approval"
	StepSleep    = "sleep"
)

// OracleStep is one step of a case's scripted solution: a shell line run as the worker (a plain string, or
// {"run": s, "exit": n}), the final answer text ({"answer": s}), a line the approval gate must stop
// ({"approval": s}, exit 3 expected, observed as an approval), or a pause ({"sleep": seconds}).
type OracleStep struct {
	Kind    string
	Line    string
	Exit    int
	Seconds float64
}

// UnmarshalJSON reads one oracle step in either form.
func (s *OracleStep) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		s.Kind = StepRun
		return json.Unmarshal(b, &s.Line)
	}
	var m struct {
		Run      *string  `json:"run"`
		Exit     *int     `json:"exit"`
		Answer   *string  `json:"answer"`
		Approval *string  `json:"approval"`
		Sleep    *float64 `json:"sleep"`
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil {
		return fmt.Errorf("an oracle step is a line, or {\"run\"|\"answer\"|\"approval\"|\"sleep\": ...}")
	}
	set := 0
	for _, p := range []bool{m.Run != nil, m.Answer != nil, m.Approval != nil, m.Sleep != nil} {
		if p {
			set++
		}
	}
	if set != 1 || (m.Exit != nil && m.Run == nil) {
		return fmt.Errorf("an oracle step has exactly one of run, answer, approval or sleep (exit goes with run)")
	}
	switch {
	case m.Run != nil:
		s.Kind, s.Line = StepRun, *m.Run
		if m.Exit != nil {
			s.Exit = *m.Exit
		}
	case m.Answer != nil:
		s.Kind, s.Line = StepAnswer, *m.Answer
	case m.Approval != nil:
		s.Kind, s.Line, s.Exit = StepApproval, *m.Approval, 3
	default:
		s.Kind, s.Seconds = StepSleep, *m.Sleep
	}
	return nil
}

// MarshalJSON writes the step back in its object form (a plain run as its line).
func (s OracleStep) MarshalJSON() ([]byte, error) {
	switch s.Kind {
	case StepRun:
		if s.Exit == 0 {
			return json.Marshal(s.Line)
		}
		return json.Marshal(map[string]any{"run": s.Line, "exit": s.Exit})
	case StepAnswer:
		return json.Marshal(map[string]string{"answer": s.Line})
	case StepApproval:
		return json.Marshal(map[string]string{"approval": s.Line})
	}
	return json.Marshal(map[string]float64{"sleep": s.Seconds})
}

// Withhold replaces every oracle secret of the suite in text, so no stored row keeps one.
func (s Suite) Withhold(text string) string {
	for _, c := range s.Cases {
		for _, sec := range c.OracleSecrets {
			if sec != "" {
				text = strings.ReplaceAll(text, sec, "[withheld]")
			}
		}
	}
	return text
}

// Case returns the case with the given id.
func (s Suite) Case(id string) (Case, bool) {
	for _, c := range s.Cases {
		if c.ID == id {
			return c, true
		}
	}
	return Case{}, false
}

// CaseBudget is the case's budget: the suite's, lowered by the case's own.
func (s Suite) CaseBudget(c Case) Budget {
	b := s.Budget
	if c.Budget.MaxTokens > 0 && (b.MaxTokens == 0 || c.Budget.MaxTokens < b.MaxTokens) {
		b.MaxTokens = c.Budget.MaxTokens
	}
	if c.Budget.MaxMinutes > 0 && (b.MaxMinutes == 0 || c.Budget.MaxMinutes < b.MaxMinutes) {
		b.MaxMinutes = c.Budget.MaxMinutes
	}
	return b
}

// Hosts is every web host the suite's mocks answer for, in order and without repeats.
func (s Suite) Hosts() []string {
	seen := map[string]bool{}
	var out []string
	names := make([]string, 0, len(s.Mocks))
	for n := range s.Mocks {
		names = append(names, n)
	}
	sortStrings(names)
	for _, n := range names {
		for _, h := range s.Mocks[n].Hosts {
			if !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
	return out
}

// HasRequiredFact reports whether any of the case's facts is required.
func (c Case) HasRequiredFact() bool {
	for _, f := range c.Facts {
		if f.IsRequired() {
			return true
		}
	}
	return false
}

// Fact returns the case's fact with the given id.
func (c Case) Fact(id string) (Fact, bool) {
	for _, f := range c.Facts {
		if f.ID == id {
			return f, true
		}
	}
	return Fact{}, false
}
