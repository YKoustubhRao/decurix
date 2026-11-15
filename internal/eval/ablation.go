// Ablation arms: an arm that switches one mechanism of the worker off, to measure what it buys (docs/evals.md,
// "Ablation arms"). A mechanism maps only to a switch the desktop's agent already reads; this file never adds one.
package eval

import (
	"sort"
	"strings"
)

// Switch is one agent setting that turns a mechanism off: the environment variable the agent reads, the value that
// means off, and where the agent reads it (so a test can hold the agent to it).
type Switch struct {
	Env   string
	Value string
	// ReadIn is the agent file that reads Env; TestEveryAblationSwitchIsReadByTheAgent checks it does, and that the
	// file says Value turns the mechanism off.
	ReadIn string
	// What is switched off, in a sentence for the table.
	What string
}

// Mechanisms are the mechanisms an ablation arm may switch off (`<model>[+expert]-<mechanism>`), each by a switch the
// agent already honours, with the value its code says means off.
var Mechanisms = map[string]Switch{
	// playbooks.py: "0 switches playbooks off: nothing is kept and nothing is offered."
	"playbooks": {Env: "DECURIX_PLAYBOOKS", Value: "0", ReadIn: "agent/decurix_agent/playbooks.py",
		What: "playbooks (nothing kept, nothing offered)"},
	// skill_task._spot_every: "0 never".
	"spot-judge": {Env: "DECURIX_SPOT_JUDGE_EVERY", Value: "0", ReadIn: "agent/decurix_agent/skill_task.py",
		What: "the spot judgement of trusted replays"},
	// escalation.Ladder: calls_left starts at MODEL_CALLS; at 0 no rung asks the model.
	"heal-model": {Env: "DECURIX_ESCALATION_MODEL_CALLS", Value: "0", ReadIn: "agent/decurix_agent/computer/escalation.py",
		What: "the healing ladder's model rungs"},
}

// Unswitchable are mechanisms people ask to ablate that have no switch the agent honours, and why: an arm naming one
// is refused, never mapped to something near it.
var Unswitchable = map[string]string{
	"replay":        "the agent has no switch that turns replay off (only its timings: DECURIX_REPLAY_*)",
	"long-requests": "planned runs of long requests have no switch",
	"memory":        "DECURIX_MEMORY_CONTEXT_CHARS=0 still shows one remembered entry, so it is not off",
	"tool-direct":   "DECURIX_TOOL_DIRECT_AFTER counts runs before a tool runs directly; no value means never",
	"skill-trust":   "DECURIX_SKILL_TRUST_RUNS counts runs before trust; no value means never",
	"repairs":       "DECURIX_REPAIR_ACTIONS is at least 3, never 0",
	"plan-ask":      "DECURIX_PLAN_ASK_FIRST is set by soul per command, not by the desktop",
	"soul":          "soul's decisions are switched on by +soul, not off",
}

// MechanismNames lists the mechanisms an arm may switch off, sorted.
func MechanismNames() []string {
	out := make([]string, 0, len(Mechanisms))
	for name := range Mechanisms {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// offSwitch is the switch of an arm's Off ("" and true for none), and false for a mechanism with no switch.
func offSwitch(off string) (Switch, bool) {
	if off == "" {
		return Switch{}, true
	}
	sw, ok := Mechanisms[off]
	return sw, ok
}

// offRefusal is the sentence an arm switching off an unknown or unswitchable mechanism is refused with.
func offRefusal(arm, off string) string {
	why := "it is not a mechanism this runner knows"
	if u, ok := Unswitchable[off]; ok {
		why = u
	}
	return "Arm " + arm + ": " + clipText(off, 40) + " cannot be switched off (" + why + "); the mechanisms that can are " +
		strings.Join(MechanismNames(), ", ") + "."
}

// switches is the NAME=value list an arm's practice desktop is made with (desktop.PracticeSpec.Switches).
func (a Arm) switches() []string {
	if sw, ok := offSwitch(a.Off); ok && sw.Env != "" {
		return []string{sw.Env + "=" + sw.Value}
	}
	return nil
}
