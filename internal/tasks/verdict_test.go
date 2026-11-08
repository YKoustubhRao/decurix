package tasks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func verdictJSON(verdict, by string, results ...string) json.RawMessage {
	checks := []map[string]any{}
	for i, r := range results {
		checks = append(checks, map[string]any{"id": "c" + string(rune('a'+i)), "result": r, "basis": "code", "detail": "d", "refs": []string{}})
	}
	basis := []string{"code"}
	b, _ := json.Marshal(map[string]any{"schema": VerdictSchema, "verdict": verdict, "by": by, "why": "w",
		"basis": basis, "checks": checks, "evidence": []string{"Checked against the files: 3 rows"}})
	return b
}

func TestParseVerdict(t *testing.T) {
	if v, p := ParseVerdict(nil); v != nil || p != nil {
		t.Fatalf("no verdict = %+v %v", v, p)
	}
	if v, p := ParseVerdict(verdictJSON("done", "verify", "pass")); len(p) != 0 || v.Verdict != GateDone {
		t.Fatalf("a valid done = %+v %v", v, p)
	}
	for name, raw := range map[string]json.RawMessage{
		"done with no check":         verdictJSON("done", "verify"),
		"done with a failed check":   verdictJSON("done", "verify", "pass", "fail"),
		"done with a check not made": verdictJSON("done", "skill_gate", "pass", "not_made"),
		"an unknown verdict":         verdictJSON("probably", "verify", "pass"),
		"an unknown gate":            verdictJSON("not_done", "vibes"),
		"an unknown result":          verdictJSON("not_done", "verify", "maybe"),
		"another schema":             json.RawMessage(`{"schema":"decurix.verdict/v9","verdict":"done","by":"verify"}`),
		"not an object":              json.RawMessage(`"done"`),
		"a legacy done":              verdictJSON("done", "legacy"),
		"a legacy done with a check": verdictJSON("done", "legacy", "pass"),
		"a legacy done with a fail":  verdictJSON("done", "legacy", "fail"),
		"too big to read":            json.RawMessage(`{"schema":"decurix.verdict/v1","verdict":"unchecked","by":"none","later":"` + strings.Repeat("x", 70000) + `"}`),
	} {
		v, p := ParseVerdict(raw)
		if len(p) == 0 || v == nil || v.Verdict != GateUnchecked || !strings.Contains(v.Why, "could not be read") {
			t.Errorf("%s: %+v %v", name, v, p)
		}
	}
	// A string over the limit is clipped at a character boundary, never a reason to drop the verdict.
	long := strings.Repeat("₹", 601)
	v, notes := ParseVerdict(json.RawMessage(`{"schema":"decurix.verdict/v1","verdict":"unchecked","by":"none","why":"` + long + `"}`))
	if v == nil || v.Why != strings.Repeat("₹", 600) || len(notes) != 1 || !strings.Contains(notes[0], "clipped") {
		t.Fatalf("an over-long why = %q %v", v.Why, notes)
	}
	many := make([]map[string]any, 65)
	for i := range many {
		many[i] = map[string]any{"id": "x", "result": "pass", "basis": "code"}
	}
	b, _ := json.Marshal(map[string]any{"schema": VerdictSchema, "verdict": "done", "by": "verify", "checks": many})
	if _, p := ParseVerdict(b); len(p) == 0 {
		t.Error("65 checks were accepted")
	}
}

// The stored checked flag follows the verdict, and is false whenever the callback's own flag disagrees (fail
// closed); with no verdict (an image from before R6) a checked=True is stored not checked (fail closed), and a false or
// absent flag is kept.
func TestCheckedFollowsTheVerdict(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name    string
		verdict json.RawMessage
		checked *bool
		want    *bool
	}{
		{"done, checked", verdictJSON("done", "verify", "pass"), &yes, &yes},
		{"done, not checked: they disagree", verdictJSON("done", "verify", "pass"), &no, &no},
		{"unchecked, no flag", verdictJSON("unchecked", "none"), nil, &no},
		{"stopped, checked", verdictJSON("stopped", "worker", "pass"), &yes, &no},
		{"an old image, checked", nil, &yes, &no},
		{"an old image, not checked", nil, &no, &no},
		{"an old image, nothing said", nil, nil, nil},
	}
	for _, c := range cases {
		s := openStore(t, newClock())
		route, _ := s.Commit(NewRoute(), Worker{ID: "box", Name: "box"}, "count the rows", Origin{Kind: OriginPerson})
		v, _ := ParseVerdict(c.verdict)
		task, exec, err := s.RecordOutcomeExec("box", route.TaskID, Outcome{Kind: OutcomeDone, Reply: "3", Checked: c.checked, Verdict: v})
		if err != nil || task != route.TaskID || exec != route.ExecutionID {
			t.Fatalf("%s: RecordOutcomeExec = %q %q %v", c.name, task, exec, err)
		}
		d, _ := s.Get(route.TaskID)
		e := d.Executions[0]
		got := e.Checked
		if (got == nil) != (c.want == nil) || got != nil && *got != *c.want {
			t.Errorf("%s: checked = %v, want %v", c.name, ptrString(got), ptrString(c.want))
		}
		if (e.Verdict == nil) != (c.verdict == nil) {
			t.Errorf("%s: verdict read back = %+v", c.name, e.Verdict)
		}
		if c.name == "stopped, checked" && e.WorkDone() {
			t.Errorf("a stop is the work done")
		}
		if c.name == "done, checked" && (!e.WorkDone() || e.Verdict.Evidence[0] != "Checked against the files: 3 rows") {
			t.Errorf("a checked done = %+v", e)
		}
	}
}

func ptrString(b *bool) string {
	if b == nil {
		return "nil"
	}
	if *b {
		return "true"
	}
	return "false"
}

// The shared fixture (also read by agent/tests): non-ASCII text at exactly 600 characters, more bytes, is read
// whole and valid (review CP-B B1: Go counted bytes, Python characters).
func TestTheSharedUnicodeVerdictIsRead(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "verdict-unicode-600.json"))
	if err != nil {
		t.Fatal(err)
	}
	v, notes := ParseVerdict(raw)
	if len(notes) != 0 || v.Verdict != GateDone || utf8.RuneCountInString(v.Why) != 600 || len(v.Why) <= 600 {
		t.Fatalf("the fixture = %q (%d characters) %v", v.Verdict, utf8.RuneCountInString(v.Why), notes)
	}
}

// The verdicts the control plane's own graders record are valid, and say what the grade was.
func TestAGraderVerdictIsValid(t *testing.T) {
	for result, want := range map[string]string{"pass": GateDone, "fail": GateNotDone, "not_made": GateUnchecked, "": GateUnchecked} {
		v := GraderVerdict(ByReadiness, result, "code", "readiness.verify", "checked by the readiness verifier", []string{"readiness:r1"})
		raw, _ := json.Marshal(v)
		got, notes := ParseVerdict(raw)
		if len(notes) != 0 || got.Verdict != want {
			t.Errorf("%q: %q %v", result, got.Verdict, notes)
		}
	}
}

// A stop is never the work done, even on a row an older rule stored checked (WorkDone's defence in depth).
func TestWorkDoneRefusesAStop(t *testing.T) {
	yes := true
	e := Execution{Status: StatusDone, Checked: &yes, Verdict: &GateVerdict{Verdict: GateStopped}}
	e.Outcome = DisplayOutcome(e.Status, e.Checked)
	if e.WorkDone() {
		t.Fatal("a stop stored checked is the work done")
	}
	e.Verdict = nil
	if !e.WorkDone() {
		t.Fatal("a checked done with no verdict (an old row) is not the work done")
	}
}

// codeDone is a valid checked done (one code check passed), for a test that needs an outcome stored checked: a bare
// checked=True with no verdict is stored not checked.
func codeDone() *GateVerdict {
	return GraderVerdict("verify", "pass", "code", "test.check", "checked in the test", nil)
}

// A row stored with a legacy verdict (before every desktop path gave its own) is still read back as it was stored.
func TestAStoredLegacyVerdictIsStillRead(t *testing.T) {
	s := openStore(t, newClock())
	route, _ := s.Commit(NewRoute(), Worker{ID: "box", Name: "box"}, "count the rows", Origin{Kind: OriginPerson})
	no := false
	if _, err := s.RecordOutcome("box", route.TaskID, Outcome{Kind: OutcomeDone, Reply: "3", Checked: &no}); err != nil {
		t.Fatal(err)
	}
	old := `{"schema":"decurix.verdict/v1","verdict":"done","by":"legacy","why":"","basis":[],"checks":[],"evidence":[]}`
	if _, err := s.db.Exec(`UPDATE executions SET verdict = ?, checked = 1 WHERE id = ?`, old, route.ExecutionID); err != nil {
		t.Fatal(err)
	}
	d, err := s.Get(route.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	e := d.Executions[0]
	if e.Verdict == nil || e.Verdict.By != "legacy" || e.Verdict.Verdict != GateDone || e.Checked == nil || !*e.Checked {
		t.Fatalf("an old legacy row read back as %+v checked %v", e.Verdict, ptrString(e.Checked))
	}
}
