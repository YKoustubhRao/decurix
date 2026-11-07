package tasks

import "testing"

func TestDisplayOutcomeNeverCallsAnUncheckedResultDone(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		status  string
		checked *bool
		state   string
		label   string
	}{
		{StatusDone, &yes, StateDoneChecked, "Done"},
		{StatusDone, &no, StateDoneUnchecked, "Done, not checked"},
		{StatusDone, nil, StateDoneUnchecked, "Done, not checked"},
		{StatusWaiting, nil, StateNeedsYou, "Waiting on you"},
		{StatusError, &yes, StateFailed, "Failed"},
		{StatusAbandoned, nil, StateStopped, "Stopped"},
		{StatusQuery, nil, StateStopped, "Ended waiting"},
		{StatusRunning, nil, StateRunning, "Running"},
		{StatusQueued, nil, StateQueued, "Queued"},
		{"something-new", &yes, "something-new", "something-new"},
	}
	for _, c := range cases {
		got := DisplayOutcome(c.status, c.checked)
		if got.State != c.state || got.Label != c.label {
			t.Errorf("DisplayOutcome(%q, %v) = %+v, want %s %q", c.status, c.checked, got, c.state, c.label)
		}
		if got.Done() != (c.state == StateDoneChecked) {
			t.Errorf("DisplayOutcome(%q, %v).Done() = %v", c.status, c.checked, got.Done())
		}
	}
}

// What the records hand every page - a task, its executions, the list and a card's dropdown - carries the outcome
// with the final check's word in it.
func TestEveryReadCarriesTheCheckedOutcome(t *testing.T) {
	c := newClock()
	s := openStore(t, c)
	box := worker("box", "thebot")

	no := false
	unchecked := send(t, s, box, "file the return")
	outcome(t, s, "box", unchecked.TaskID, Outcome{Kind: OutcomeDone, Reply: "filed", Checked: &no})

	yes := true
	checked := send(t, s, box, "count the rows")
	outcome(t, s, "box", checked.TaskID, Outcome{Kind: OutcomeDone, Reply: "12", Checked: &yes, Verdict: codeDone()})

	silent := send(t, s, box, "say hi")
	outcome(t, s, "box", silent.TaskID, Outcome{Kind: OutcomeDone, Reply: "hi"})

	want := map[string]string{
		unchecked.TaskID: StateDoneUnchecked,
		checked.TaskID:   StateDoneChecked,
		silent.TaskID:    StateDoneUnchecked,
	}

	for id, state := range want {
		detail, err := s.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if detail.Outcome.State != state || detail.Executions[0].Outcome.State != state {
			t.Errorf("Get(%s) outcome = %s / %s, want %s", id, detail.Outcome.State, detail.Executions[0].Outcome.State, state)
		}
	}

	list, err := s.List(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, summary := range list {
		if summary.Outcome.State != want[summary.ID] {
			t.Errorf("List %s outcome = %s, want %s", summary.ID, summary.Outcome.State, want[summary.ID])
		}
	}

	recent, err := s.RecentForWorker("box", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range recent {
		if task.Outcome.State != want[task.ID] {
			t.Errorf("RecentForWorker %s outcome = %s, want %s", task.ID, task.Outcome.State, want[task.ID])
		}
	}
}

// A reason read from words is labelled a guess; a stop and a wrong verdict are facts (C-22).
func TestWordReadReasonsAreMarkedGuessed(t *testing.T) {
	if !GuessedReason(ReasonApprovalPayment) || !GuessedReason(ReasonCredentials) || GuessedReason(ReasonStopped) || GuessedReason(ReasonWrong) {
		t.Fatal("GuessedReason misjudges which reasons are read from words")
	}
}
