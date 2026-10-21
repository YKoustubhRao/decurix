package skills

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The named-app nudges across the six live requests (live_requests.json in DECURIX_TEST_FIXTURES, shared with the
// atomizer's and the agent's corpus tests): a stage whose own deliverable the request binds to an app is sent to that
// app when it was made with tools only; every other stage - P1's workbook beside its "deck in PowerPoint", P4's letters
// from "the template (Word)", P5's files "if you use the Office apps on my PC" - is never sent to an app, with or
// without a shared folder. Found live (CL, round 5 P1): the workbook stage was sent to PowerPoint.
func TestLiveRequestsNudgeOnlyTheBoundDeliverable(t *testing.T) {
	// Outside the repository (it quotes the person's requests - never committed); the test skips when it is not here.
	dir := os.Getenv("DECURIX_TEST_FIXTURES")
	if dir == "" {
		dir = "/mnt/c/Users/Administrator/decurix-tests/fixtures"
		if _, err := os.Stat(dir); err != nil {
			dir = `C:\Users\Administrator\decurix-tests\fixtures`
		}
	}
	path := filepath.Join(dir, "live_requests.json")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("fixture not here: %s (set DECURIX_TEST_FIXTURES)", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Requests []struct {
			ID           string `json:"id"`
			Text         string `json:"text"`
			Deliverables []struct {
				File string `json:"file"`
				App  string `json:"app"`
			} `json:"deliverables"`
		} `json:"requests"`
	}
	if err := json.Unmarshal(raw, &corpus); err != nil || len(corpus.Requests) == 0 {
		t.Fatalf("the corpus: %v", err)
	}
	for _, shared := range []string{"", `C:\Users\Administrator\Documents\Decurix demo`} {
		t.Setenv("DECURIX_SHARED_FOLDER", shared)
		for _, r := range corpus.Requests {
			for i, d := range r.Deliverables {
				// The stage's prompt as long_task.stage_prompt opens it, with the app line the agent decided.
				lines := []string{"Stage " + strconv.Itoa(i+1) + " of " + strconv.Itoa(len(r.Deliverables)) +
					" of a longer request a person made: make " + d.File + ".", "Hands in: " + d.File}
				if d.App != "" {
					lines = append(lines, "Work in app: "+d.App)
				} else {
					lines = append(lines, "No app is named for what this stage hands in: make it with your own tools.")
				}
				st := &operatorState{Request: strings.Join(lines, "\n") + "\n\nThe request:\n" + r.Text,
					Worked: []string{"office create /home/headless/work/" + d.File + " - <<'EOF'\n{}\nEOF"}}
				nudge := sharedAppUntouchedNudge(st)
				if d.App == "" && nudge != "" {
					t.Errorf("%s %s (shared %q): sent to an app it is not bound to: %q", r.ID, d.File, shared, nudge[:80])
				}
				if d.App != "" && !strings.Contains(nudge, "no window of "+strings.ToLower(d.App)+" was opened") {
					t.Errorf("%s %s (shared %q): made with tools only, not sent to %s: %q", r.ID, d.File, shared, d.App, nudge)
				}
				st.NamedAppOpened = true
				unsaved := sharedAppUnsavedNudge(st)
				if (d.App == "") != (unsaved == "") {
					t.Errorf("%s %s (shared %q): the unsaved nudge = %q", r.ID, d.File, shared, unsaved)
				}
			}
		}
	}
}
