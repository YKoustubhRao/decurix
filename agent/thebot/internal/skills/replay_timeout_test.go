// A replay of a taught task or a recording run as one command gets the replay's limit, not one command's three
// minutes: found live (2026-11-08), a 52-step taught task was killed at step 19 and left a form half filled.
package skills

import (
	"testing"
	"time"
)

func TestAReplayGetsTheReplaysLimitAndOtherCommandsTheirOwn(t *testing.T) {
	t.Setenv("OPERATOR_COMMAND_TIMEOUT_SECONDS", "")
	t.Setenv("OPERATOR_REPLAY_TIMEOUT_SECONDS", "")
	o := &OperatorSkill{}
	for _, command := range []string{
		"gui do enter-an-order --param 'customer=Acme' --param qty=2",
		"  gui replay demo-20261108T151704-e5c4f2 --from 4",
		"cd ~/work && gui do enter-an-order",
	} {
		if got := o.timeoutFor(command); got != defaultReplayTimeout {
			t.Errorf("timeoutFor(%q) = %v, want %v", command, got, defaultReplayTimeout)
		}
	}
	for _, command := range []string{"gui click e12", "gui observe", "echo gui do x", "gui skill enter-an-order"} {
		if got := o.timeoutFor(command); got != defaultCommandTimeout {
			t.Errorf("timeoutFor(%q) = %v, want %v", command, got, defaultCommandTimeout)
		}
	}
}

func TestTheReplayLimitNeverShortensALongerCommandLimit(t *testing.T) {
	o := &OperatorSkill{CommandTimeout: 30 * time.Minute}
	if got := o.timeoutFor("gui do enter-an-order"); got != 30*time.Minute {
		t.Errorf("a replay under a 30-minute command limit got %v", got)
	}
	t.Setenv("OPERATOR_REPLAY_TIMEOUT_SECONDS", "600")
	short := &OperatorSkill{CommandTimeout: time.Minute}
	if got := short.timeoutFor("gui replay demo-1"); got != 10*time.Minute {
		t.Errorf("OPERATOR_REPLAY_TIMEOUT_SECONDS=600 gave %v", got)
	}
}

// A choice from a list gets its own longer limit: its searches bound it, not the shell's three minutes.
func TestASelectGetsTheSelectLimit(t *testing.T) {
	o := &OperatorSkill{}
	if got := o.timeoutFor(`gui select e12 "Developer Workstation"`); got != defaultSelectTimeout {
		t.Fatalf("select limit %v", got)
	}
	if got := o.timeoutFor(`gui click e12`); got != defaultCommandTimeout {
		t.Fatalf("click limit %v", got)
	}
}
