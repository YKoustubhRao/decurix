package tasks

import (
	"encoding/json"
	"strings"
	"testing"
)

// Review CP-B B1: Python cuts every verdict string to 600 characters (verdict.py _text: [:MAX_TEXT]); Go must accept what
// Python sends. A why of 600 characters with one non-ASCII character in it is valid on both sides.
func TestAVerdictPythonSendsIsRead(t *testing.T) {
	why := strings.Repeat("a", 598) + "₹—" // 600 characters, 604 bytes
	b, _ := json.Marshal(map[string]any{"schema": VerdictSchema, "verdict": "done", "by": "verify", "why": why,
		"basis": []string{"code"}, "checks": []map[string]any{{"id": "files.read_back", "result": "pass", "basis": "code",
			"detail": "ok", "refs": []string{}}}, "evidence": []string{}})
	v, p := ParseVerdict(b)
	if len(p) != 0 || v.Verdict != GateDone {
		t.Fatalf("a valid Python verdict was stored as %q: %v", v.Verdict, p)
	}
}

// Review CP-B B4 (H-PY-1): a "legacy" done with a failed check must never be stored checked (and since every desktop path
// gives its own verdict, "legacy" is no gate at all: any legacy done is refused).
func TestALegacyDoneWithAFailedCheckIsNotDone(t *testing.T) {
	b, _ := json.Marshal(map[string]any{"schema": VerdictSchema, "verdict": "done", "by": "legacy", "why": "",
		"basis": []string{"code"}, "checks": []map[string]any{{"id": "files.read_back", "result": "fail", "basis": "code",
			"detail": "the total differs", "refs": []string{}}}, "evidence": []string{}})
	v, p := ParseVerdict(b)
	if len(p) == 0 && v.Verdict == GateDone {
		t.Fatalf("a legacy done with a failed check and basis code was accepted as done")
	}
}
