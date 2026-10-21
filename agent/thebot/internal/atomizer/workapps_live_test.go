package atomizer

import (
	"reflect"
	"testing"
)

// Round 5 P4 (2026-10-20): Word named as a template's format and as an option was taken as "work in Word", and the
// operator was refused office update for the letters.
func TestAnAppNamedAsAFormatOrAnOptionIsNotWhereTheWorkIsDone(t *testing.T) {
	notApps := []string{
		"2. An offer letter for each person from the template (Word), every placeholder filled, with the CTC and the " +
			"monthly break-up as per the policy.",
		"Save the files in P4_onboarding_Nov in your work folder (or in Documents\\Decurix demo\\process\\P4 on my PC " +
			"if you use Word there).",
		"Make his order sheet in Excel, wa_orders.xlsx in my work folder.",
		"An Excel file of the totals, please.",
		"A Word document with the minutes.",
		"You can use Excel or anything else to make the summary.",
		"Make the deck, in PowerPoint if you like.",
	}
	for _, p := range notApps {
		if got := WorkApps(p); len(got) != 0 {
			t.Errorf("%q named %v as where to work", p, got)
		}
	}
	apps := map[string][]string{
		"Use Excel (the Excel app on your desktop) to open receivables.xlsx and add a column.": {"excel"},
		"Open it in Word and save it as letter.docx.":                                          {"word"},
		"Make the chart in Excel, then use PowerPoint for the deck.":                           {"excel", "powerpoint"},
		"Enter the vouchers on my PC in Tally, then make the summary in Calc.":                 {"calc"},
	}
	for p, want := range apps {
		if got := WorkApps(p); !reflect.DeepEqual(got, want) {
			t.Errorf("%q = %v, want %v", p, got, want)
		}
	}
}
