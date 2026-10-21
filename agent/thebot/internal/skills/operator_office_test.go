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

// Found live (round 5 P6, 2026-10-20, twice): an email with no order number was left "Need order lookup" though the
// system's order list had the sender's company and the product. The task rules say to find it by what is given.
func TestOperatorContractFindsARecordByWhatTheMessageGives(t *testing.T) {
	for _, want := range []string{
		"when its source gives no reference is found by what the source does give",
		"search or filter the system's list",
		"only when several match or none does",
	} {
		if !strings.Contains(taskRules, want) {
			t.Errorf("the operator's task rules do not say %q", want)
		}
	}
}

// Found live (round 5 P1, 2026-10-20): a bill's code wrapped under its row was paired with the next row's rate, and
// two bills were flagged with GST rate mismatches they did not have.
func TestOperatorTaskRulesReadAWrappedRowAsItsOwn(t *testing.T) {
	for _, want := range []string{
		"a code on such a line belongs to the row above",
		"that row's figures are its own",
	} {
		if !strings.Contains(taskRules, want) {
			t.Errorf("the operator's task rules do not say %q", want)
		}
	}
}
