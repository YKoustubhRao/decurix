package tasks

// What a person is shown about how a task or an attempt stands. The records keep the raw status and, apart from it,
// whether the desktop's final check judged a finished result (checked). "Done" alone is only the worker's word: a
// result its own check did not judge is shown as done but not checked, everywhere, from this one function - the task
// page, the list, the dashboard, the finished notification, the concierge and the DAG all read it (review 2026-11-03,
// W-01: every page said Done for work the gate had refused to vouch for).

// Display states.
const (
	StateDoneChecked   = "done_checked"
	StateDoneUnchecked = "done_unchecked"
	StateNeedsYou      = "needs_you"
	StateFailed        = "failed"
	StateStopped       = "stopped"
	StateRunning       = "running"
	StateQueued        = "queued"
)

// Display tones, which a page maps to its colours: ok is green, warn amber, attention the accent, bad red.
const (
	ToneOK        = "ok"
	ToneWarn      = "warn"
	ToneAttention = "attention"
	ToneBad       = "bad"
	ToneMuted     = "muted"
)

// Display is how a task or an execution stands, as a person reads it.
type Display struct {
	State string `json:"state"`
	Label string `json:"label"`
	Tone  string `json:"tone"`
}

// Done reports whether this is a finished result whose final check judged it: the only "done" another worker, a DAG
// node or a report may build on.
func (d Display) Done() bool { return d.State == StateDoneChecked }

// Finished reports whether the worker said it finished, checked or not.
func (d Display) Finished() bool {
	return d.State == StateDoneChecked || d.State == StateDoneUnchecked
}

// DisplayOutcome derives what a person is shown from a raw status and the final check's word: "done" is
// done_checked only when checked is true, and a nil checked (the desktop said nothing) is not checked.
func DisplayOutcome(status string, checked *bool) Display {
	switch status {
	case StatusDone:
		if checked != nil && *checked {
			return Display{State: StateDoneChecked, Label: "Done", Tone: ToneOK}
		}
		return Display{State: StateDoneUnchecked, Label: "Done, not checked", Tone: ToneWarn}
	case StatusWaiting:
		return Display{State: StateNeedsYou, Label: StatusLabel(status), Tone: ToneAttention}
	case StatusError:
		return Display{State: StateFailed, Label: StatusLabel(status), Tone: ToneBad}
	case StatusQuery, StatusAbandoned:
		return Display{State: StateStopped, Label: StatusLabel(status), Tone: ToneBad}
	case StatusRunning:
		return Display{State: StateRunning, Label: StatusLabel(status), Tone: ToneMuted}
	case StatusQueued:
		return Display{State: StateQueued, Label: StatusLabel(status), Tone: ToneMuted}
	default:
		// A status this build does not know is shown as it is, never as a success.
		return Display{State: status, Label: StatusLabel(status), Tone: ToneMuted}
	}
}

// FinishedPhrase is how a notification ends "<desktop> ..." for a finished task: whether its check judged it.
func FinishedPhrase(checked *bool) string {
	if checked != nil && *checked {
		return "finished its task"
	}
	return "finished its task, not checked"
}
