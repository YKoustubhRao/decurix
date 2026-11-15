// Tests that a command's full output kept on disk (keepOutput, the "(full output: PATH)" a digest points at) holds
// none of the job's secret values nor what soul knows is secret: it is a copy soul writes down.
package skills

import (
	"os"
	"strings"
	"testing"

	"thebot/internal/withheld"
)

func TestAKeptFullOutputHoldsNoSecret(t *testing.T) {
	st := &operatorState{OutputsDir: t.TempDir(), jobSecrets: withheld.Clean([]string{"larch-meadow-19"}),
		Request: "the password is Swordfish42"}
	p := st.keepOutput("row 1: larch-meadow-19\nrow 2: Swordfish42\nrow 3: plain")
	if p == "" {
		t.Fatal("nothing kept")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if strings.Contains(got, "larch-meadow-19") || strings.Contains(got, "Swordfish42") {
		t.Fatalf("a secret was kept on disk: %q", got)
	}
	if !strings.Contains(got, "row 3: plain") {
		t.Fatalf("the rest of the output was lost: %q", got)
	}
}
