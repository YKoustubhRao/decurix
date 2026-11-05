package tasks

import (
	"encoding/json"
	"strings"
	"testing"
)

// A verdict from a newer desktop keeps the fields this version does not know (a v1 reader of a v1.1 verdict).
func TestAVerdictKeepsFieldsItDoesNotKnow(t *testing.T) {
	in := `{"schema":"decurix.verdict/v1","verdict":"done","by":"verify","why":"w","basis":["code"],` +
		`"checks":[{"id":"files.read_back","result":"pass","basis":"code","detail":"d","refs":["file:a"]}],` +
		`"evidence":["e"],"later":{"x":1}}`
	var v GateVerdict
	if err := json.Unmarshal([]byte(in), &v); err != nil {
		t.Fatal(err)
	}
	if v.Verdict != GateDone || len(v.Checks) != 1 || v.Checks[0].Basis != "code" {
		t.Fatalf("read %+v", v)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"later":{"x":1}`) || !strings.Contains(string(out), `"verdict":"done"`) {
		t.Fatalf("written back as %s", out)
	}
	plain, _ := json.Marshal(GateVerdict{Schema: VerdictSchema, Verdict: GateStopped})
	if strings.Contains(string(plain), "extra") {
		t.Fatalf("an unexported field leaked: %s", plain)
	}
}
