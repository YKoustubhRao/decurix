package skills

import (
	"fmt"
	"os"
	"testing"
)

// TestMeasureTheFixedPrompt prints what every operator turn re-sends before any command output: the contract with a
// hosted brain's rules. Run with MEASURE=1 (go test -run TestMeasureTheFixedPrompt -v).
func TestMeasureTheFixedPrompt(t *testing.T) {
	if os.Getenv("MEASURE") == "" {
		t.Skip("set MEASURE=1 to print the sizes")
	}
	contract := fmt.Sprintf(operatorContract, "/home/headless/work", rulesFor("")+visionRule, "", "")
	fmt.Printf("contract without rules: %d chars\n", len(fmt.Sprintf(operatorContract, "/home/headless/work", "", "", "")))
	fmt.Printf("taskRules: %d chars\nsharedRule: %d\nvisionRule: %d\ncontract+rules+vision: %d chars (~%d tokens)\n",
		len(taskRules), len(sharedRule(`C:\Users\x\Documents\Shared`)), len(visionRule), len(contract), len(contract)/4)
}
