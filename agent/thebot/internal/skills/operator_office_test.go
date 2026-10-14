package skills

import (
	"strings"
	"testing"
)

// Found live (2026-10-14): asked for a review deck, the operator had no tool
// that made a .pptx and nothing told it how to change one it was given. The
// contract names both, the way it names the workbook and document commands.
func TestOperatorContractTeachesMakingAndChangingADeck(t *testing.T) {
	for _, want := range []string{
		"office create <file.pptx> - with {\"slides\":",
		"office update <file.pptx> - and {\"edits\":",
		"\"chart\": {\"type\": \"column\"",
		"never rebuild a deck someone gave you",
		"PowerPoint, email",
	} {
		if !strings.Contains(operatorContract, want) {
			t.Errorf("the operator's contract does not say %q", want)
		}
	}
}
