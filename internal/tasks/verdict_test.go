package tasks

import (
	"encoding/json"
	"strings"
	"testing"
)

func verdictJSON(verdict, by string, results ...string) json.RawMessage {
	checks := []map[string]any{}
	for i, r := range results {
		checks = append(checks, map[string]any{"id": "c" + string(rune('a'+i)), "result": r, "basis": "code", "detail": "d", "refs": []string{}})
	}
	b, _ := json.Marshal(map[string]any{"schema": VerdictSchema, "verdict": verdict, "by": by, "why": "w",
		"basis": []string{"code"}, "checks": checks, "evidence": []string{"Checked against the files: 3 rows"}})
	return b
}

func TestParseVerdict(t *testing.T) {
	if v, p := ParseVerdict(nil); v != nil || p != nil {
		t.Fatalf("no verdict = %+v %v", v, p)
	}
	if v, p := ParseVerdict(verdictJSON("done", "verify", "pass")); len(p) != 0 || v.Verdict != GateDone {
		t.Fatalf("a valid done = %+v %v", v, p)
	}
	// A legacy done (a final path with no verdict of its own, derived on the desktop) carries no checks (H-PY-1).
	if v, p := ParseVerdict(verdictJSON("done", "legacy")); len(p) != 0 || v.Verdict != GateDone {
		t.Fatalf("a legacy done = %+v %v", v, p)
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
		"a why too long":             json.RawMessage(`{"schema":"decurix.verdict/v1","verdict":"unchecked","by":"none","why":"` + strings.Repeat("x", 601) + `"}`),
	} {
		v, p := ParseVerdict(raw)
		if len(p) == 0 || v == nil || v.Verdict != GateUnchecked || !strings.Contains(v.Why, "could not be read") {
			t.Errorf("%s: %+v %v", name, v, p)
		}
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
// closed); with no verdict (an older image) it is the callback's flag, exactly as before.
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
		{"an old image, checked", nil, &yes, &yes},
		{"an old image, nothing said", nil, nil, nil},
	}
	for _, c := range cases {
		s := openStore(t, newClock())
		route, _ := s.Commit(NewRoute(), Worker{ID: "box", Name: "box"}, "count the rows")
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
