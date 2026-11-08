package skills

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"thebot/internal/llm"
)

// A plan put to the person that says "Then, each with your OK first: Press Save bill" is kept: the press waits for
// the gate's yes even where the gate would not ask on its own (found live 2026-11-08: the worker pressed Save bill
// after its own pre-save check and never asked).

func TestThePlanReadsEachCommitWithTheControlItIsMadeWith(t *testing.T) {
	plan, ok := parsePlan(`The plan: {"scope": "the bill", "sources": ["bill.pdf"], "outputs": ["the bill on the page"],
		"commits": [{"what": "Press Save bill", "control": " Save  bill "}, "file the return", {"control": "Send"}, {"what": ""}],
		"ask": ""}`)
	if !ok {
		t.Fatal("the plan was not read")
	}
	if want := []string{"Press Save bill", "file the return", "Send"}; !reflect.DeepEqual(plan.Commits, want) {
		t.Errorf("commits = %q, want %q", plan.Commits, want)
	}
	if want := []string{"Save bill", "", "Send"}; !reflect.DeepEqual(plan.Controls, want) {
		t.Errorf("controls = %q, want %q", plan.Controls, want)
	}
	if _, ok := parsePlan(`{"scope": " ", "commits": []}`); ok {
		t.Error("a plan with no scope was read")
	}
	// The shape the planner used before: plain strings, no control known.
	if plan, ok := parsePlan(aPlan); !ok || !reflect.DeepEqual(plan.Commits, []string{"file the return"}) ||
		!reflect.DeepEqual(plan.Controls, []string{""}) {
		t.Errorf("an older plan was read as %+v", plan)
	}
}

const okFirstPlan = `{"scope": "the bill in bill.pdf", "sources": ["bill.pdf"], "outputs": ["the bill on the bill-entry page"],
	"commits": [{"what": "Press Save bill", "control": "Save bill"}], "ask": ""}`

func planEnv(st *operatorState) string {
	for _, e := range st.guiEnv() {
		if strings.HasPrefix(e, planAskFirstEnv+"=") {
			return e
		}
	}
	return ""
}

func TestAPlanPutToThePersonWithItsOKFirstStepsIsHeldInEveryGuiCommand(t *testing.T) {
	st := &operatorState{Hosted: true, Request: "enter this bill into the bill-entry page"}
	q := (&OperatorSkill{}).planBeforeChange(weighRC(&scriptedChat{replies: []string{okFirstPlan}}), st, `gui type e5 "4120"`)
	if !strings.Contains(q, "each with your OK first: Press Save bill") || st.Plan == nil || !st.Plan.Promised {
		t.Fatalf("question %q, plan %+v", q, st.Plan)
	}
	if got := planEnv(st); got != planAskFirstEnv+`=["Save bill"]` {
		t.Errorf("gui commands are told %q", got)
	}
	// A step whose control the plan did not know holds every committing press ("").
	st.Plan.Commits = append(st.Plan.Commits, "file the return")
	if got := planEnv(st); got != planAskFirstEnv+`=["Save bill",""]` {
		t.Errorf("gui commands are told %q", got)
	}

	// Never put to the person (a method for the kind of task is known, or a stage of an agreed plan): no promise was
	// made, and the gui commands behave as before.
	stage := &operatorState{Hosted: true, Request: "Stage 1 of 2: enter the bill", PlanAgreed: true}
	(&OperatorSkill{}).planBeforeChange(weighRC(&scriptedChat{replies: []string{okFirstPlan}}), stage, `gui type e5 "4120"`)
	if stage.Plan != nil && stage.Plan.Promised || planEnv(stage) != "" {
		t.Errorf("a plan nobody was shown holds presses: %+v", stage.Plan)
	}
	// A plan with no commits, or one kept from before this field (no promise recorded), holds nothing.
	for _, p := range []*workPlan{{Scope: "a report", Promised: true}, {Scope: "the bill", Commits: []string{"save the bill"}}} {
		if got := planEnv(&operatorState{Plan: p}); got != "" {
			t.Errorf("plan %+v: gui commands are told %q", p, got)
		}
	}
}

// savePressStub is a gui whose press is held for approval while the plan's hold is in its environment and no grant
// exists (as core.act holds it, _plan_hold), and which notes each real press in pressed.
func savePressStub(t *testing.T) (grant, pressed string) {
	dir := t.TempDir()
	grant, pressed = filepath.ToSlash(filepath.Join(dir, "granted")), filepath.ToSlash(filepath.Join(dir, "pressed"))
	stubWorkerTools(t,
		`if [ -n "$`+planAskFirstEnv+`" ] && [ ! -f `+grant+` ]; then `+
			`echo 'NEEDS_APPROVAL: Approval needed before I click button "Save bill" (e33) in "Bills": the plan of the work you agreed to makes "Save bill" with your OK first. Nothing was clicked or sent.'; exit 3; fi`+"\n"+
			`echo pressed >> `+pressed+"\necho 'Clicked button \"Save bill\" (e33).'\n",
		"", "")
	return grant, pressed
}

func startWithPlan(t *testing.T, chat *scriptedChat, plan *workPlan) (*OperatorSkill, RunContext, Subtask, *operatorState) {
	op := NewOperator()
	rc := operatorContext(t, chat)
	subtask := Subtask{Step: "save the bill that is open on the bill page", Type: "operator"}
	st := op.start(rc, subtask)
	st.Plan = plan
	return op, rc, subtask, st
}

func promisedPlan() *workPlan {
	return &workPlan{Scope: "the bill", Commits: []string{"Press Save bill"}, Controls: []string{"Save bill"}, Promised: true}
}

func TestAPromisedSaveWaitsForTheYesAndIsPressedAfterIt(t *testing.T) {
	grant, pressed := savePressStub(t)
	chat := &scriptedChat{replies: []string{"COMMAND:\ngui click e33 --checked", "DONE:\nThe bill was saved."}}
	op, rc, subtask, st := startWithPlan(t, chat, promisedPlan())
	res, _ := op.loop(rc, subtask, st)
	if res.Meta["needsApproval"] != true || res.Meta["action"] != "gui click e33 --checked" || res.Checkpoint == nil {
		t.Fatalf("result = %+v, want the press held for approval", res)
	}
	if reason, _ := res.Meta["approvalReason"].(string); !strings.Contains(reason, `click button "Save bill"`) {
		t.Errorf("approvalReason = %q, want the gate's words for the press", reason)
	}
	if _, err := os.Stat(pressed); err == nil {
		t.Fatal("Save bill was pressed before the person's yes")
	}

	if err := os.WriteFile(grant, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	rc.Resume, rc.HumanReply = res.Checkpoint, "yes"
	rc.Approved = func(action string) bool { return action == "gui click e33 --checked" }
	res, _ = op.Run(rc, subtask)
	if !res.Verified || res.Output != "The bill was saved." {
		t.Fatalf("resumed result = %+v", res)
	}
	if _, err := os.Stat(pressed); err != nil {
		t.Fatal("the approved press did not run")
	}
	if done := res.Checkpoint; done != nil {
		if s, ok := done.(*operatorState); ok && s.commitsIssue() != nil {
			t.Error("a commit made on the person's yes was not counted")
		}
	}
}

func TestARefusedPromisedSaveIsNotPressedAndTheReportIsSentBack(t *testing.T) {
	_, pressed := savePressStub(t)
	chat := &scriptedChat{replies: []string{"COMMAND:\ngui click e33 --checked", "DONE:\nI left the bill unsaved."}}
	op, rc, subtask, st := startWithPlan(t, chat, promisedPlan())
	res, _ := op.loop(rc, subtask, st)
	if res.Meta["needsApproval"] != true {
		t.Fatalf("result = %+v, want the press held for approval", res)
	}
	held := res.Checkpoint.(*operatorState)
	rc.Resume, rc.HumanReply = res.Checkpoint, "no, do not save it yet"
	if _, err := op.Run(rc, subtask); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pressed); err == nil {
		t.Fatal("Save bill was pressed after the person said no")
	}
	if !slices.ContainsFunc(chat.calls[1], func(m llm.Message) bool { return strings.Contains(m.Content, "did not approve") }) {
		t.Error("the model was not told the press was not approved")
	}
	// The plan's step did not go through: a report of the work done is sent back, not taken (commitsIssue).
	if held.CommitsRun != 0 || held.commitsIssue() == nil {
		t.Errorf("the refused step counted as done (run %d)", held.CommitsRun)
	}
}

func TestWithoutAPromiseTheSaveGoesAsBefore(t *testing.T) {
	_, pressed := savePressStub(t)
	chat := &scriptedChat{replies: []string{"COMMAND:\ngui click e33 --checked", "DONE:\nThe bill was saved."}}
	plan := promisedPlan()
	plan.Promised = false
	op, rc, subtask, st := startWithPlan(t, chat, plan)
	res, _ := op.loop(rc, subtask, st)
	if res.Meta["needsApproval"] == true || !res.Verified {
		t.Fatalf("result = %+v, want the press made unasked as before", res)
	}
	if _, err := os.Stat(pressed); err != nil {
		t.Fatal("the press did not run")
	}
}

func TestAHaltedReplaysEntriesAreCreditedToTheDocumentTheyCameFrom(t *testing.T) {
	st := &operatorState{}
	st.addUnit("document", "invoice2.pdf", "Invoice INV-2 Amount 7350.50 Tax 661.55 GSTIN 29ABCDE1234F1Z5")
	halted := "halted: Stopped replaying bill-entry at step 11: button \"Save bill\" is not on the screen. 10 step(s) ran before it, and nothing after it was done.\n" +
		replayEnteredMarker + ` ["7350.50", "661.55"]` + "\nTo carry on after putting it right: gui do bill-entry --from 11\n"
	if gaps := st.coverageGaps(); len(gaps) != 1 || !strings.Contains(gaps[0], "none of its figures went in") {
		t.Fatalf("before the replay, gaps = %q", gaps)
	}
	// From a chain, the line could be a file's: not credited.
	st.noteReplayEntered("gui do bill-entry && cat notes.txt", halted)
	if len(st.Written) != 0 {
		t.Fatalf("a chain's output was credited: %q", st.Written)
	}
	st.noteReplayEntered("gui do bill-entry --source-file invoice2.pdf", halted)
	if gaps := st.coverageGaps(); len(gaps) != 0 {
		t.Errorf("figures the halted replay entered were not credited: %q", gaps)
	}
}
