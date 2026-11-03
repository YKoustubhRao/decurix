package tasks

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// A run's final check reaches the control plane structured (plan R6, decurix.verdict/v1, C5): the verdict, which
// gate gave it, the kinds of check that passed and each check with its result. The store keeps it beside the old
// checked flag, which is then only derived from it: checked = (verdict == "done"), and never true when the callback's
// own flag says otherwise (fail closed).

// Limits of a verdict (PLAN-P2 §5.2).
const (
	maxVerdictChecks = 64
	maxCheckRefs     = 16
	maxVerdictString = 600 // characters (runes), as Python counts them, never bytes
	// maxVerdictBytes bounds the raw verdict before it is read at all: its unknown fields are kept, so they are
	// bounded too (review CP-B N5).
	maxVerdictBytes = 64 << 10
)

// verdictBy are the gates a verdict may come from; "legacy" is a final path that gave no verdict of its own and was
// derived from its checked flag on the desktop (H-PY-1): its done carries no checks and no basis. "readiness",
// "practice" and "eval" are the control plane's own graders (a readiness run, a Simulate case, an eval case), which
// record their grade as a verdict like any gate.
var verdictBy = map[string]bool{"verify": true, "skill_gate": true, "long_task": true, "replay": true, "workflow": true,
	"worker": true, "none": true, "legacy": true, ByReadiness: true, ByPractice: true, ByEval: true}

// The control plane's own graders, as a verdict's by.
const (
	ByReadiness = "readiness"
	ByPractice  = "practice"
	ByEval      = "eval"
)

var verdictKinds = map[string]bool{GateDone: true, GateNotDone: true, GateUnchecked: true, GateStopped: true}

var checkResults = map[string]bool{"pass": true, "fail": true, "not_made": true}

// checkBases are the kinds of evidence a check rests on.
var checkBases = map[string]bool{"code": true, "ledger": true, "screen": true, "model": true, "person": true}

// unreadableVerdict is what a verdict that does not validate is stored as.
func unreadableVerdict() *GateVerdict {
	return &GateVerdict{Schema: VerdictSchema, Verdict: GateUnchecked, By: "none",
		Why: "the desktop's verdict could not be read", Basis: []string{}, Checks: []CheckResult{}, Evidence: []string{}}
}

// ParseVerdict reads a final callback's verdict. It returns nil for none (an older image), the verdict when it
// validates, and otherwise an unchecked verdict saying it could not be read, with the problems found. A string longer
// than the limit is clipped at a character boundary, never a reason to drop the verdict; the clip is among the notes
// returned (with an empty verdict problem list it stays valid). Fields it does not know are kept.
func ParseVerdict(raw json.RawMessage) (*GateVerdict, []string) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if len(raw) > maxVerdictBytes {
		return unreadableVerdict(), []string{fmt.Sprintf("the verdict is %d bytes, more than %d", len(raw), maxVerdictBytes)}
	}
	var v GateVerdict
	if err := json.Unmarshal(raw, &v); err != nil {
		return unreadableVerdict(), []string{"not a verdict object: " + err.Error()}
	}
	clipped := v.clipStrings()
	if problems := v.Problems(); len(problems) > 0 {
		return unreadableVerdict(), append(problems, clipped...)
	}
	if v.Basis == nil {
		v.Basis = []string{}
	}
	if v.Checks == nil {
		v.Checks = []CheckResult{}
	}
	if v.Evidence == nil {
		v.Evidence = []string{}
	}
	return &v, clipped
}

// clipVerdictText is s cut to maxVerdictString characters at a character boundary, and whether it was cut.
func clipVerdictText(s string) (string, bool) {
	if utf8.RuneCountInString(s) <= maxVerdictString {
		return s, false
	}
	n := 0
	for i := range s {
		if n == maxVerdictString {
			return s[:i], true
		}
		n++
	}
	return s, false
}

// clipStrings cuts every string of v to the limit, and says which it cut.
func (v *GateVerdict) clipStrings() []string {
	var notes []string
	clip := func(field string, s *string) {
		if cut, ok := clipVerdictText(*s); ok {
			*s = cut
			notes = append(notes, field+" was clipped to "+fmt.Sprint(maxVerdictString)+" characters")
		}
	}
	clip("why", &v.Why)
	for i := range v.Checks {
		c := &v.Checks[i]
		clip(fmt.Sprintf("check %d id", i), &c.ID)
		clip(fmt.Sprintf("check %d detail", i), &c.Detail)
		for j := range c.Refs {
			clip(fmt.Sprintf("check %d ref", i), &c.Refs[j])
		}
	}
	for i := range v.Evidence {
		clip("an evidence line", &v.Evidence[i])
	}
	return notes
}

// Problems is what makes v not a valid decurix.verdict/v1: an unknown schema, verdict, gate, result or basis, a
// count limit passed, a string over the limit (in characters), a done with a failed or unmade check (under any gate),
// a done with no passing check, or a legacy done that carries checks or a basis.
func (v GateVerdict) Problems() []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	long := func(field, s string) {
		if utf8.RuneCountInString(s) > maxVerdictString {
			add("%s is longer than %d characters", field, maxVerdictString)
		}
	}
	if v.Schema != VerdictSchema {
		add("schema %q is not %s", v.Schema, VerdictSchema)
	}
	if !verdictKinds[v.Verdict] {
		add("verdict %q is not one of done, not_done, unchecked, stopped", v.Verdict)
	}
	if !verdictBy[v.By] {
		add("by %q is not a known gate", v.By)
	}
	long("why", v.Why)
	for _, b := range v.Basis {
		if !checkBases[b] {
			add("basis %q is not a known kind of check", b)
		}
	}
	if len(v.Checks) > maxVerdictChecks {
		add("%d checks, more than %d", len(v.Checks), maxVerdictChecks)
	}
	passed, failed := 0, 0
	for i, c := range v.Checks {
		if !checkResults[c.Result] {
			add("check %d: result %q is not pass, fail or not_made", i, c.Result)
		}
		if !checkBases[c.Basis] {
			add("check %d: basis %q is not a known kind of check", i, c.Basis)
		}
		if len(c.Refs) > maxCheckRefs {
			add("check %d: %d refs, more than %d", i, len(c.Refs), maxCheckRefs)
		}
		long(fmt.Sprintf("check %d id", i), c.ID)
		long(fmt.Sprintf("check %d detail", i), c.Detail)
		for _, r := range c.Refs {
			long(fmt.Sprintf("check %d ref", i), r)
		}
		switch c.Result {
		case "pass":
			passed++
		case "fail", "not_made":
			failed++
		}
	}
	if len(v.Evidence) > maxVerdictChecks {
		add("%d evidence lines, more than %d", len(v.Evidence), maxVerdictChecks)
	}
	for _, e := range v.Evidence {
		long("an evidence line", e)
	}
	if v.Verdict == GateDone {
		switch {
		case failed > 0:
			add("done with a check failed or not made")
		case v.By == "legacy" && (len(v.Checks) > 0 || len(v.Basis) > 0):
			add("a legacy done carries no checks and no basis")
		case v.By != "legacy" && passed == 0:
			add("done needs at least one passing check")
		}
	}
	return out
}

// GraderVerdict is the verdict of one of the control plane's own graders (by: ByReadiness, ByPractice, ByEval):
// result "pass" is done, "fail" not done, "not_made" unchecked, resting on basis (code for a grader that read the
// state back), with one check named id. Every control-plane producer records its grade so, never as a bare checked.
func GraderVerdict(by, result, basis, id, detail string, refs []string) *GateVerdict {
	verdict := GateUnchecked
	switch result {
	case "pass":
		verdict = GateDone
	case "fail":
		verdict = GateNotDone
	default:
		result = "not_made"
	}
	passed := []string{}
	if result == "pass" {
		passed = []string{basis}
	}
	if refs == nil {
		refs = []string{}
	}
	detail, _ = clipVerdictText(detail)
	return &GateVerdict{Schema: VerdictSchema, Verdict: verdict, By: by, Why: detail, Basis: passed,
		Checks:   []CheckResult{{ID: id, Result: result, Basis: basis, Detail: detail, Refs: refs}},
		Evidence: []string{}}
}

// WorkDone reports whether an execution is the work done: shown as done and checked, and not a stop at a person's no
// (a stop's report can be checked, but the work it was asked for was not done). checkedFrom already stores a stop
// unchecked; the stopped clause is defence in depth for a row checked by an older rule (TestWorkDoneRefusesAStop).
func (e Execution) WorkDone() bool {
	return e.Outcome.Done() && (e.Verdict == nil || e.Verdict.Verdict != GateStopped)
}

// checkedFrom is the checked flag an outcome is stored with: what its verdict says (done), never true when the
// callback's own flag disagrees; the callback's flag alone when there is no verdict (an older image).
func checkedFrom(v *GateVerdict, claimed *bool) (*bool, bool) {
	if v == nil {
		return claimed, false
	}
	checked := v.Verdict == GateDone
	disagree := claimed != nil && *claimed != checked
	if disagree {
		checked = false
	}
	return &checked, disagree
}
