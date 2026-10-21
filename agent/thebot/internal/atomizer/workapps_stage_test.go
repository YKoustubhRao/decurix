package atomizer

import (
	"reflect"
	"testing"
)

// Found live (round 5 P1, 2026-10-20): "3. A 5-slide MIS deck in PowerPoint ... 4. A Word memo" held the workbook
// stage to PowerPoint. A stage is held only to the app its prompt names for its own output.
func TestAppsForBindsANamedAppToItsDeliverable(t *testing.T) {
	request := "1. One workbook, month_end_close_Oct2026.xlsx in P1_month_end/outputs, with these sheets.\n" +
		"3. A 5-slide MIS deck in PowerPoint: title; sales and GST.\n4. A Word memo of the exceptions.\n" +
		"Save the deck as MIS_Oct2026.pptx and the memo as Exceptions_memo_Oct2026.docx in Documents\\Decurix demo\\process\\P1 on my PC."
	stage := func(lines string) string {
		return "Stage 1 of 4 of a longer request a person made: x.\n" + lines + "\n\nThe request:\n" + request
	}
	cases := []struct {
		prompt string
		want   []string
	}{
		{stage("Hands in: P1_month_end/outputs/month_end_close_Oct2026.xlsx\nNo app is named for what this stage hands in."), nil},
		{stage("Hands in: PC/process/P1/MIS_Oct2026.pptx\nWork in app: PowerPoint"), []string{"powerpoint"}},
		{stage("Hands in: PC/process/P1/Exceptions_memo_Oct2026.docx\nWork in app: Word"), []string{"word"}},
		{request, []string{"powerpoint"}}, // a task that is no stage: as before
	}
	for _, c := range cases {
		if got := AppsFor(c.prompt); !reflect.DeepEqual(got, c.want) {
			t.Errorf("AppsFor(%q...) = %v, want %v", c.prompt[:90], got, c.want)
		}
	}
}
