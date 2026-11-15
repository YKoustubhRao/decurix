// Tests that an eval ablation arm's switches reach only its practice desktop's environment, and only in their one
// allowed shape (a DECURIX_ name, a number).
package desktop

import (
	"slices"
	"testing"
)

func TestPracticeSwitchesAreSetOnlyInTheirShape(t *testing.T) {
	env := practiceEnvironment(nil, PracticeSpec{RunID: "r1", Harness: "thebot", Brain: "azure",
		Switches: []string{"DECURIX_PLAYBOOKS=0", "DECURIX_SPOT_JUDGE_EVERY=0", "PATH=/tmp", "DECURIX_X=abc",
			"DECURIX_AGENT_TOKEN=1", "decurix_lower=1", "DECURIX_Y=1;rm"}}, "pw")
	if envValue(env, "DECURIX_PLAYBOOKS") != "0" || envValue(env, "DECURIX_SPOT_JUDGE_EVERY") != "0" {
		t.Fatalf("the switches were not set: %v", env)
	}
	for _, bad := range []string{"PATH=/tmp", "DECURIX_X=abc", "DECURIX_AGENT_TOKEN=1", "decurix_lower=1", "DECURIX_Y=1;rm"} {
		if slices.Contains(env, bad) {
			t.Fatalf("%q reached the environment: %v", bad, env)
		}
	}
	plain := practiceEnvironment(nil, PracticeSpec{RunID: "r1", Harness: "thebot", Brain: "azure"}, "pw")
	if envValue(plain, "DECURIX_PLAYBOOKS") != "" {
		t.Fatalf("a switch is set without an arm asking: %v", plain)
	}
}
