package skills

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// E (false-done investigation): a Save the application asked no approval for still went through. The ledger read it
// back, so the plan's commit is counted and commitsIssue raises no false "none of them went through".
func TestASaveThatNeededNoApprovalCountsAsTheCommit(t *testing.T) {
	plan := &workPlan{Scope: "enter the bill", Commits: []string{"save the bill"}}
	saved := shellResult{ExitCode: 0, Output: "Clicked button \"Save\"\nRecord FJ-1190: saved; the page now shows 6 of 6 declared value(s) - all of them."}

	st := &operatorState{Plan: plan}
	st.noteUngatedCommit(`gui click e20`, saved)
	if st.CommitsRun != 1 || len(st.CommitsDone) != 1 || !strings.Contains(st.CommitsDone[0], "FJ-1190") {
		t.Fatalf("the save was not counted: run %d, done %v", st.CommitsRun, st.CommitsDone)
	}
	if got := st.commitsIssue(); got != nil {
		t.Fatalf("a false point after a save that went through: %+v", got)
	}

	for name, c := range map[string]struct {
		st      *operatorState
		command string
		res     shellResult
	}{
		"a failed command":                 {&operatorState{Plan: plan}, `gui click e20`, shellResult{ExitCode: 1, Output: saved.Output}},
		"no read-back, no save":            {&operatorState{Plan: plan}, `gui click e21`, shellResult{Output: "Clicked button \"Next\""}},
		"not a gui command":                {&operatorState{Plan: plan}, `echo 'Record X: saved'`, shellResult{Output: "Record X: saved"}},
		"no plan with commits":             {&operatorState{Plan: &workPlan{Scope: "a report"}}, `gui click e20`, saved},
		"the person's OK, counted already": {&operatorState{Plan: plan, approvedRun: `gui click e20`}, `gui click e20`, saved},
	} {
		c.st.noteUngatedCommit(c.command, c.res)
		if c.st.CommitsRun != 0 {
			t.Errorf("%s: counted as a commit", name)
		}
	}
	// Saving from the app in front (Ctrl+S) counts too.
	st = &operatorState{Plan: plan}
	st.noteUngatedCommit(`gui key ctrl+s`, shellResult{Output: "Pressed ctrl+s"})
	if st.CommitsRun != 1 {
		t.Fatal("a save from the app was not counted")
	}
	// With nothing that went through, the point is still raised.
	if got := (&operatorState{Plan: plan}).commitsIssue(); len(got) != 1 {
		t.Fatalf("the point is gone when nothing went through: %+v", got)
	}
}

// A (false-done investigation): a call the worker marks "| not backed" is an open point for the person at a report,
// decided in code, not a fact the review may let stand; the review is shown it as already open.
func TestANotBackedCallIsAnOpenPointNotAFact(t *testing.T) {
	items := []string{
		`Vendor "Finchmoor Joinery Works" | backed by: "Finchmoor Joinery Works"`,
		`Cost centre set to "General" | not backed: the source document gives no cost centre, "General" was the default`,
	}
	issues := notBackedIssues(items)
	if len(issues) != 1 || issues[0].Settle != "person" || !strings.Contains(issues[0].What, "Cost centre") {
		t.Fatalf("issues = %+v", issues)
	}

	st := &operatorState{Request: "enter bill.pdf"}
	facts := st.reviewFacts("Done.", items, "")
	if !strings.Contains(facts, "ALREADY OPEN, DECIDED IN CODE") || !strings.Contains(facts, `Cost centre set to "General"`) {
		t.Fatalf("the review is not told the call is already open:\n%s", facts)
	}
	if i := strings.Index(facts, "DECISIONS WHOSE CITED WORDS"); i >= 0 && strings.Contains(facts[i:], "Cost centre") &&
		!strings.Contains(facts[strings.Index(facts, "ALREADY OPEN"):], "Cost centre") {
		t.Fatalf("the call is shown only as a fact:\n%s", facts)
	}

	// At a DONE the weighing raises it, though no review ran (no model): it is the code's point.
	o := &OperatorSkill{}
	w := o.weighJudgements(weighRC(&scriptedChat{}), &operatorState{Hosted: true, Request: "enter bill.pdf"}, items, "Done.", "")
	if !strings.Contains(w.nudge+w.question+strings.Join(w.open, "\n"), "Cost centre") {
		t.Fatalf("the not-backed call was not raised: %+v", w)
	}
}

// E, through execute: a gui save whose output is the ledger's read-back counts as the plan's commit.
func TestExecuteCountsASaveThatNeededNoApproval(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a stand-in gui on PATH")
	}
	bin := t.TempDir()
	script := "#!/bin/sh\necho 'Clicked button \"Save\"'\necho 'Record FJ-1190: saved; the page now shows 6 of 6 declared value(s) - all of them.'\n"
	if err := os.WriteFile(filepath.Join(bin, "gui"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	st := &operatorState{Plan: &workPlan{Scope: "enter the bill", Commits: []string{"save the bill"}}}
	rc := operatorContext(t, &scriptedChat{})
	if blocked := NewOperator().execute(rc, st, "gui click e20"); blocked != "" {
		t.Fatalf("blocked: %s", blocked)
	}
	if st.CommitsRun != 1 || st.commitsIssue() != nil {
		t.Fatalf("the save through execute was not counted: %d", st.CommitsRun)
	}
}
