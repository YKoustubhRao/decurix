package atomizer

import (
	"reflect"
	"testing"
)

func TestADesksJobIsNeverTheCodingHarnesss(t *testing.T) {
	// Found live (2026-10-19): this went to the coding harness, which built a tested Python package; no summary existed.
	for _, p := range []string{
		`Look through app.log in the "Decurix demo" folder in my Documents (C:\Users\Administrator\Documents\Decurix demo) (open it in Notepad on your desktop) and write a summary in Notepad, saved in the same folder as error_summary.txt: one line per service in the form service: count, counting the ERROR-level lines, most errors first.`,
		`Count the ERROR lines per service in app.log and write the counts to error_summary.txt.`,
	} {
		if r := Triage(p); r.Kind == RouteSkill && r.TaskType == "coding" {
			t.Errorf("went to the coding harness: %q", p)
		}
	}
	if !worksAtTheDesk(`Resize logo.png to 50% in Paint (on your desktop).`) || worksAtTheDesk(`Build a Python CLI that parses logs and prints counts per service.`) {
		t.Error("worksAtTheDesk misread a request")
	}
}

func TestWorkAppsTellsTheAppToWorkInFromAFilesFormat(t *testing.T) {
	cases := []struct {
		prompt string
		want   []string
	}{
		// Found live (2026-10-19): the app named next to the file asked for is the file's format.
		{"Make his order sheet in Excel, wa_orders.xlsx in my work folder: one row per item.", nil},
		{"Put the totals in Excel (totals.xlsx) in my work folder.", nil},
		{"Send me the list in Excel format.", nil},
		{"I need the register in an Excel file.", nil},
		// Where to work, said outright: these stay on the screen.
		{"Use Excel (the Excel app on your desktop) to open sales_q3.csv from my work folder.", []string{"excel"}},
		{"Use LibreOffice Calc to open the attached sales register.", []string{"calc"}},
		{"Open the attached sales_q3.csv in Excel. Sort the rows by Amount.", []string{"excel"}},
		{"In LibreOffice Writer, write a reply letter to the customer.", []string{"writer"}},
		{"Make a deck from the attached brief, built in LibreOffice Impress: a title slide.", []string{"impress"}},
		{"Write payment reminder letters in Word (the Word app on your desktop).", []string{"word"}},
		{"Make a chart using PowerPoint, then save it as chart.pptx.", []string{"powerpoint"}},
		// A file of another kind after the app is not that app's format.
		{"Build the chart in Excel, notes.txt has the figures.", []string{"excel"}},
		{"Make a list of the files in my work folder.", nil},
	}
	for _, c := range cases {
		if got := WorkApps(c.prompt); !reflect.DeepEqual(got, c.want) {
			t.Errorf("WorkApps(%q) = %v, want %v", c.prompt, got, c.want)
		}
	}
}
