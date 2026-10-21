package skills

import (
	"strings"
	"testing"
)

const sharedHost = `C:\Users\Administrator\Documents\Decurix demo`

// Without a shared folder the rule for a named app is exactly what it was; with one, the contract says one thing:
// make the content with tools in the folder, then open, check and save it in the named app. Found live (2026-10-20):
// the old "never behind the app's back" won over the shared-folder line, and a chart was built by hand until the
// step budget ran out.
func TestTheContractSaysOneThingAboutANamedAppWithAndWithoutASharedFolder(t *testing.T) {
	if rulesFor("") != taskRules {
		t.Fatal("the rules changed for a desktop with no shared folder")
	}
	if !strings.Contains(taskRules, behindTheAppSentence) {
		t.Fatal("the named-app rule is no longer in taskRules: rulesFor replaces nothing")
	}

	shared := rulesFor(sharedHost) + sharedRule(sharedHost)
	if strings.Contains(shared, "check the result, never make the deliverable behind the app's back.") &&
		!strings.Contains(shared, "Anywhere else, office and python may read sources and check the result, never make the deliverable behind the app's back.") {
		t.Error("a shared-folder desktop is still told never to make the deliverable with its tools")
	}
	for _, want := range []string{
		"make its content there with office or python first",
		"open that file in the named app",
		"save it from the app (Save, keeping its name and format)",
		"Never build by hand in the app what office create or update makes in one command",
	} {
		if !strings.Contains(shared, want) {
			t.Errorf("the shared-folder rules lack %q", want)
		}
	}
	rule := sharedRule(sharedHost)
	// sharedRule says where the folder is; the named-app procedure has one home, sharedAppSentence (harness audit,
	// 2026-10-21: the shared-folder desktop read it twice).
	if !strings.Contains(rule, "the named-app and source-file rules above say how") ||
		strings.Contains(rule, "save it from the app") {
		t.Errorf("sharedRule repeats the named-app procedure, or does not point to it: %q", rule)
	}
	// Generic: no product named in either line.
	for _, name := range []string{"Excel", "Word", "PowerPoint", "Calc", "Impress", "Writer"} {
		if strings.Contains(sharedAppSentence, name) || strings.Contains(rule, name) {
			t.Errorf("a product name, %s, in the shared-folder rules", name)
		}
	}
}

// A source is opened, never saved (found live 2026-10-20: the person's input workbook was saved with a new column 50 s
// after the result, most likely when the app holding it closed).
func TestTheContractSaysASourceIsNeverSaved(t *testing.T) {
	for _, rules := range []string{rulesFor(""), rulesFor(sharedHost)} {
		if !strings.Contains(rules, "A source file (one the request gives you to read or work from) is opened, never saved") ||
			!strings.Contains(rules, "choose not to save") {
			t.Error("taskRules lacks the source-file line")
		}
	}
	if strings.Contains(sharedRule(sharedHost), "open a source, never save it") {
		t.Error("the shared-folder line repeats the source-file rule taskRules holds")
	}
}

func TestSavingFromTheAppIsSeen(t *testing.T) {
	for _, c := range []struct{ command, output string }{
		{"gui key ctrl+s", "key ctrl+s - title changed"},
		{"gui key F12", "key f12 - window \"Save As\" opened"},
		{"gui click e42", `click button "Save" (e42) in "chart - PowerPoint"`},
		{"gui click e7", `clicked menu item "Save As" (e7)`},
	} {
		if !savedInApp(c.command, c.output) {
			t.Errorf("not seen as a save from the app: %s / %s", c.command, c.output)
		}
	}
	for _, c := range []struct{ command, output string }{
		{"office update /home/headless/PC/x.xlsx - <<'EOF'\n{}\nEOF", `click button "Save"`},
		{"gui click e3", `click button "Save to cloud" (e3)`},
		{"gui key ctrl+c", "copied"},
	} {
		if savedInApp(c.command, c.output) {
			t.Errorf("taken for a save from the app: %s / %s", c.command, c.output)
		}
	}
}

// Made with tools in the shared folder, then opened, checked and saved in the named app: done in the app. Tools only:
// sent back to open it there. Opened but never saved from it: sent back once to save it from the app.
func TestToolsThenTheAppIsDoneInTheApp(t *testing.T) {
	t.Setenv("DECURIX_SHARED_FOLDER", sharedHost)
	request := `Make a deck in PowerPoint with a line chart of monthly sales from sales.xlsx in the "Decurix demo" folder; save it there as sales.pptx.`
	made := "office create /home/headless/PC/sales.pptx - <<'EOF'\n{\"slides\": []}\nEOF"

	toolsOnly := &operatorState{Request: request, Worked: []string{"office read /home/headless/PC/sales.xlsx", made}}
	if sharedAppUntouchedNudge(toolsOnly) == "" {
		t.Error("made with tools only, and taken")
	}

	opened := &operatorState{Request: request, Worked: []string{made, `gui open "PowerPoint (Host)"`}, NamedAppOpened: true}
	if n := sharedAppUntouchedNudge(opened); n != "" {
		t.Errorf("opened in the app, still told to open it: %q", n)
	}
	n := sharedAppUnsavedNudge(opened)
	if !strings.Contains(n, "nothing was saved from powerpoint") || !strings.Contains(n, sharedHost) {
		t.Errorf("opened but never saved from the app: %q", n)
	}

	opened.SavedInApp = true
	if n := sharedAppUnsavedNudge(opened); n != "" {
		t.Errorf("made with tools, opened, checked and saved in the app, yet sent back: %q", n)
	}

	// Work done in the app itself on the screen (no tool-made file) is not asked to be saved again by this rule.
	inApp := &operatorState{Request: request, Worked: []string{`gui open "PowerPoint (Host)"`}, NamedAppOpened: true}
	if n := sharedAppUnsavedNudge(inApp); n != "" {
		t.Errorf("work done in the app was sent back: %q", n)
	}
	// No shared folder: the same rule (Finding BQ), in its own words.
	t.Setenv("DECURIX_SHARED_FOLDER", "")
	if n := sharedAppUnsavedNudge(&operatorState{Request: request, Worked: []string{made}, NamedAppOpened: true}); !strings.Contains(n, "With the file open there (the file)") {
		t.Errorf("a desktop without a shared folder: %q", n)
	}
}

// Enter in a form's field submits the whole form (found live 2026-10-20: an order saved with one line of several).
func TestTheContractSaysEnterSubmitsAForm(t *testing.T) {
	if !strings.Contains(taskRules, "In a form, move between fields with Tab or by clicking the next field; never press Enter") {
		t.Error("taskRules lacks the form line")
	}
}

// A test record saved in a real application is a real record (found live 2026-10-20: a test order in a portal that
// cannot delete orders).
func TestTheContractSaysNeverSaveARecordToTryTheApp(t *testing.T) {
	if !strings.Contains(taskRules, "Never save, submit or post a record to find out how an application behaves") {
		t.Error("taskRules lacks the line")
	}
}

// A repeated block in a source is never totalled silently (found live 2026-10-20: a sales export repeated a week).
func TestTheContractSaysCheckRepeatsBeforeTotalling(t *testing.T) {
	if !strings.Contains(taskRules, "never total repeated rows silently") {
		t.Error("taskRules lacks the line")
	}
}

// A value the request's own inputs give is never asked of the person (found live 2026-10-20: a joiner's mobile number).
func TestTheContractSaysLookInTheInputsBeforeAsking(t *testing.T) {
	if !strings.Contains(taskRules, "Before asking the person for a value, look in the request's own input files") {
		t.Error("taskRules lacks the line")
	}
}

// A template is filled with office update, not Find & Replace on the screen (found live 2026-10-20: offer letters).
func TestTheContractSaysFillATemplateWithOfficeUpdate(t *testing.T) {
	if !strings.Contains(operatorContract, "To fill a Word document or template, run office update DOC.docx") {
		t.Error("the contract lacks the line")
	}
}

// Found live (2026-10-20, P5): an average typed as 12.67 fed a revenue per head (1078139.62 for 1078423.34), and a
// highlight said "-0.4% above target".
func TestTheContractSaysCarryFullPrecisionAndNoNegativeAbove(t *testing.T) {
	for _, want := range []string{"Never round a figure you go on to calculate with", "never a negative \"above\""} {
		if !strings.Contains(taskRules, want) {
			t.Errorf("taskRules lacks %q", want)
		}
	}
}
