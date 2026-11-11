package desktop

import (
	"slices"
	"testing"
)

// An eval arm with +soul turns on every decision soul words and reads (plan rule 3, phase 2) on its practice desktop
// only; without it, none is set, whatever the image's own environment says.
func TestPracticeSoulDecisionsAreTheArmsOnly(t *testing.T) {
	on := practiceEnvironment(nil, PracticeSpec{RunID: "r1", Harness: "thebot", Brain: "azure", SoulDecisions: true}, "pw")
	off := practiceEnvironment(nil, PracticeSpec{RunID: "r1", Harness: "thebot", Brain: "azure"}, "pw")
	for _, name := range SoulDecisionSwitches {
		if envValue(on, name) != "1" {
			t.Fatalf("%s is not on with SoulDecisions: %v", name, on)
		}
		if slices.ContainsFunc(off, func(kv string) bool { return len(kv) > len(name) && kv[:len(name)+1] == name+"=" }) {
			t.Fatalf("%s is set without SoulDecisions: %v", name, off)
		}
	}
}
