package skills

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"thebot/internal/withheld"
)

// The held-value rule shared with the control plane and the agent (internal/llmrec/testdata/held-values.json, in
// the main module): every vector, run through withholdFor with the case's values as the job's secrets, gives what the
// control plane's llmrec.ReplaceHeld and the agent's privacy.withhold_values give.
func TestHeldValueVectorsThroughWithholdFor(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "internal", "llmrec", "testdata", "held-values.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("the shared vectors are not here (%v): soul is built outside the Decurix tree", err)
	}
	var doc struct {
		Cases []struct {
			Text   string   `json:"text"`
			Values []string `json:"values"`
			Want   string   `json:"want"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Cases) < 10 {
		t.Fatalf("only %d vectors", len(doc.Cases))
	}
	for _, c := range doc.Cases {
		st := &operatorState{jobSecrets: withheld.Clean(c.Values)}
		if got := st.withholdFor("")(c.Text); got != c.Want {
			t.Errorf("%q with %q:\n  got  %q\n  want %q", c.Text, c.Values, got, c.Want)
		}
	}
}

// H-PY-9: a job value with a quote in it, written the way a shell writes it inside single quotes, leaves the step
// line withheld. (A gui type with such quoting is refused before it runs - the stdin rule - so a shell command carries it.)
func TestAShellQuotedValueLeavesTheStepLineWithheld(t *testing.T) {
	chat := &taggedChat{replies: []string{"COMMAND:\n" + `echo 'pa'\''ss-w0rd9'`, "DONE:\nechoed"}}
	ctx := withheld.With(context.Background(), []string{"pa'ss-w0rd9"})
	logged, traced := logsOf(t, chat, ctx, "type the password into the field")
	for _, part := range []string{"ss-w0rd9", "pa'"} {
		if strings.Contains(logged, part) || strings.Contains(traced, part) {
			t.Fatalf("a part of the quoted value left (%q):\n%s\n%s", part, logged, traced)
		}
	}
	if !strings.Contains(logged, "$ echo '"+withheld.Mark+"'") {
		t.Fatalf("the step line is not there, withheld:\n%s", logged)
	}
}
