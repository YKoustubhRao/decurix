package skills

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestAnAppNamedAsTheFilesFormatDoesNotKeepTheFileOffTheShell(t *testing.T) {
	// Found live (2026-10-19): sent back from writing the workbook, the operator typed it into a spreadsheet cell by
	// cell and ran out of time with nothing saved.
	st := &operatorState{Request: "Make his order sheet in Excel, wa_orders.xlsx in my work folder: one row per item, and a Total row.",
		Worked: []string{"office create /home/headless/work/wa_orders.xlsx - <<'EOF'\n{}\nEOF"}}
	if n := sharedAppUntouchedNudge(st); n != "" {
		t.Errorf("an app named only as the file's format was asked for: %q", n)
	}
	// Said outright, the app is where the result is handled - after the tools made its content.
	app := &operatorState{Request: "Use Excel (the Excel app on your desktop) to make the order sheet wa_orders.xlsx in my work folder.",
		Worked: []string{"office create /home/headless/work/wa_orders.xlsx - <<'EOF'\n{}\nEOF"}}
	if n := sharedAppUntouchedNudge(app); !strings.Contains(n, "no window of excel was opened") {
		t.Errorf("an app named to work in: %q", n)
	}
}

func TestTextGivenOnStdinIsWhatIsTypedAndIsStillTraced(t *testing.T) {
	cmd := "gui type e30 - <<'EOF'\n=IF(TRIM(D2)=\"Maharashtra\",E2*9%,0)\tTotal\nEOF\nEOF2\ngui key enter"
	got := typedValues(cmd)
	if len(got) != 2 || got[0] != `=IF(TRIM(D2)="Maharashtra",E2*9%,0)` || got[1] != "Total" {
		t.Fatalf("typedValues = %q", got)
	}
	// A body line that is one word is not the end of it; only the closing word is.
	if got := typedValues("gui type - <<EOF\nItem\tQty\nTotal\n14150\nEOF"); len(got) != 4 || got[3] != "14150" {
		t.Fatalf("typedValues = %q", got)
	}
	// The grounding guard reads the body: a heredoc is no way round it.
	st := groundingState("Add CGST 9% for customers in Maharashtra, and a Total row.")
	st.noteSeen("D2='Maharashtra'  E2=12400")
	if n := groundingNudge(st, cmd); n != "" {
		t.Errorf("a traced formula on stdin was refused: %q", n)
	}
	if n := groundingNudge(st, "gui type e30 - <<'EOF'\nGreen Furnishings\nEOF"); n == "" {
		t.Error("an invented value on stdin was let through")
	}
}

func TestQuotesTheShellWouldDropAreCaughtBeforeTyping(t *testing.T) {
	// Found live (2026-10-19): this reached the sheet as =IF(LOWER(TRIM(D2))=maharashtra,E2*9%,0).
	if n := quotesNudge(`gui type "=IF(LOWER(TRIM(D2))=""maharashtra"",E2*9%,0)"`); !strings.Contains(n, "gui type REF - <<'EOF'") {
		t.Errorf("not caught: %q", n)
	}
	for _, ok := range []string{
		`gui type e12 ""`,
		`gui type e30 "=IF(D2=\"x\",1,0)"`,
		"gui type e30 - <<'EOF'\n=IF(D2=\"\",\"\",E2)\nEOF",
		`office update a.xlsx '{"edits": [{"cell": "A1", "value": "=IF(B1="""",1,0)"}]}'`,
	} {
		if n := quotesNudge(ok); n != "" {
			t.Errorf("%s was sent back: %q", ok, n)
		}
	}
}

func TestAQuoteInsideTheTextIsCaughtWhateverItsShape(t *testing.T) {
	// Found live (2026-10-19) in real Excel: inner quotes closed and reopened the shell's quoting.
	for _, bad := range []string{
		`gui type "=IFERROR(VLOOKUP(B2,Prices!A:B,2,FALSE),"NOT FOUND")"`,
		`gui type e5035 "=IF(D2="NOT FOUND","",C2*D2)" && gui key enter`,
		`gui key escape && gui type e1 "x" && gui type "=IF(ISNUMBER(D2),C2*D2,"")"`,
		`gui type e1 '=IF(D2="x",1'`, // a formula whose brackets do not close
	} {
		if n := quotesNudge(bad); !strings.Contains(n, "verbatim on stdin") {
			t.Errorf("not caught: %s", bad)
		}
	}
	for _, ok := range []string{
		`gui type e1 'say "hi" now'`,
		`gui type e1 "Line Total" && gui key tab`,
		`gui do enter --param customer="Green Furnishings"`,
		"gui type e5035 - <<'EOF'\n=IFERROR(VLOOKUP(B2,Prices!A:C,3,FALSE),\"NOT FOUND\")\nEOF\ngui key enter",
	} {
		if n := quotesNudge(ok); n != "" {
			t.Errorf("%s was sent back: %q", ok, n)
		}
	}
}

func TestAKeyChordInAChainIsNotATypedValue(t *testing.T) {
	// Found live (2026-10-19): the chain's last word, "ctrl+d", was refused as a value nobody had given.
	cmd := `gui type e4818 "D1" && gui key enter && gui type "Unit Price" && gui key tab && gui type "Line Total" && gui key ctrl+d`
	got := typedValues(cmd)
	want := []string{"D1", "Unit Price", "Line Total"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("typedValues = %q, want %q", got, want)
	}
	st := groundingState("On the Orders sheet add a column Unit Price and a column Line Total.")
	if n := groundingNudge(st, cmd); n != "" {
		t.Errorf("a chain with a key chord was refused: %q", n)
	}
	// Each command of a chain is still checked: an invented value later in the chain is refused.
	if n := groundingNudge(st, cmd+` && gui type "Green Furnishings"`); n == "" {
		t.Error("an invented value later in a chain was let through")
	}
	// Separators inside quotes are text; a here-document is the stdin of its command, and what follows it runs on.
	segs := shellSegments("gui type e1 \"a; b && c\" | cat\ngui type e2 - <<'EOF'\nx;y\nEOF\ngui key enter")
	if len(segs) != 4 || segs[0].Text != `gui type e1 "a; b && c"` || segs[1].Text != "cat" ||
		!segs[2].HasStdin || segs[2].Stdin != "x;y" || segs[3].Text != "gui key enter" {
		t.Fatalf("segments = %+v", segs)
	}
}

func TestAPathIsTypedAsWrittenOnEveryPlatform(t *testing.T) {
	// Found live (2026-10-19): the \n and \t of a path were read as Enter and Tab, and \r of \receivables as a carriage
	// return. The Windows-only test of this never ran on a Linux desktop's soul.
	for _, path := range []string{
		`C:\Users\Administrator\Documents\Decurix demo\receivables.xlsx`,
		`C:\new\temp\notes.txt`,
		`D:\receivables\fy26\vendors\backup\archive\reports.csv`,
		`\\fileserver\share\reports\table.xlsx`,
		`%USERPROFILE%\Documents\notes\todo.txt`,
	} {
		// Single-quoted: inside "..." the shell turns \\ into \, so a share's path has to be.
		got := typedValues(`gui type e960 '` + path + `'`)
		if len(got) != 1 || got[0] != path {
			t.Errorf("typedValues(%q) = %q, want the path whole", path, got)
		}
		if w := withoutNewlineMarks(path); w != path {
			t.Errorf("withoutNewlineMarks(%q) = %q", path, w)
		}
	}
	// Outside a path the keys keep their meaning, and a file then an escape then another path splits there.
	if got := typedValues(`gui type e9 "Region\tRevenue\nNorth\t812"`); strings.Join(got, "|") != "Region|Revenue|North|812" {
		t.Errorf("a table row: %q", got)
	}
	got := typedValues(`gui type e9 "C:\data\report.csv\nC:\new\temp\c.txt"`)
	if strings.Join(got, "|") != `C:\data\report.csv|C:\new\temp\c.txt` {
		t.Errorf("two paths on two lines: %q", got)
	}
	spans := pathSpans(`see C:\Users\me\Documents\Decurix demo\receivables.xlsx please`)
	if len(spans) != 1 || !strings.Contains(`see C:\Users\me\Documents\Decurix demo\receivables.xlsx please`[spans[0][0]:spans[0][1]], "receivables.xlsx") {
		t.Errorf("spans = %v", spans)
	}
	if len(pathSpans("Note:\\nTotal, ratio 3:\\n")) != 0 {
		t.Error("prose with a colon was taken for a path")
	}
	st := groundingState(`Open C:\Users\Administrator\Documents\Decurix demo\receivables.xlsx in Excel.`)
	if n := groundingNudge(st, `gui type e960 "C:\Users\Administrator\Documents\Decurix demo\receivables.xlsx"`); n != "" {
		t.Errorf("the request's own path was refused: %q", n)
	}
}

func TestADollarTheShellWouldExpandIsCaught(t *testing.T) {
	// Found live (2026-10-19): $A$2:$C$13 inside "..." reached real Excel as ":".
	for _, bad := range []string{
		`gui type D2 "=IFERROR(VLOOKUP(B2,Prices!$A$2:$C$13,3,FALSE),0)"`,
		`gui type e9 =SUM($B2:$B9)`,
		// "$5" is the fifth positional parameter, empty: the app got "Total: " (and "Lift$2026" became "Lift026").
		`gui type e9 "Total: $5"`,
	} {
		if n := quotesNudge(bad); !strings.Contains(n, "$ before a letter") {
			t.Errorf("not caught: %s", bad)
		}
	}
	for _, ok := range []string{
		`gui type e9 '=SUM($B$2:$B$9)'`,
		"gui type D2 - <<'EOF'\n=SUM($B$2:$B$9)\nEOF",
		`gui type e9 "Total: 5 $"`,
		`gui type e9 "=SUM(\$B2:\$B9)"`,
	} {
		if n := quotesNudge(ok); n != "" {
			t.Errorf("%s was sent back: %q", ok, n)
		}
	}
	st := groundingState("Look up each SKU's price on the Prices sheet with a formula.")
	st.noteSeen("A2='SKU-100'  B2=640")
	if n := groundingNudge(st, `gui type e9 '=@IFERROR(VLOOKUP(B2,Prices!$A:$B,2,FALSE),0)'`); n != "" {
		t.Errorf("a formula read back with =@ was refused: %q", n)
	}
}

func TestFormulaSyntaxIsStructureNotData(t *testing.T) {
	// Found live (2026-10-19, real Excel): the "=" pressed on its own, then the rest typed; and a range typed into a
	// function's argument box. Both were refused as values never read, and a clerk was asked where "FALSE" came from.
	st := groundingState("On the Orders sheet add a column Unit Price that looks up each SKU's price on the Prices sheet with a formula; where a SKU is not on the price list, show NOT FOUND.")
	st.noteSeen("orders_oct.xlsx: sheets ['Orders', 'Prices']\nA2='SKU-100'  B2=640  C2=3")
	for _, ok := range []string{
		`gui type e9 'IFERROR(VLOOKUP(B2,Prices!$A:$B,2,FALSE),"NOT FOUND")'`,
		`gui type e9 "Prices!$A:$B"`,
		`gui type e9 "'Prices'!A2:C13"`,
		`gui type e9 "B:B"`,
		`gui type e9 "2:13"`,
	} {
		if n := groundingNudge(st, ok); n != "" {
			t.Errorf("%s was refused: %q", ok, n)
		}
	}
	for _, bad := range []string{`gui type e9 'IFERROR(VLOOKUP(B2,Prices!$A:$B,2,FALSE),"MISSING")'`, `gui type e9 "Green Furnishings"`} {
		if n := groundingNudge(st, bad); n == "" {
			t.Errorf("%s was let through", bad)
		}
	}
}

func TestAPersonIsAskedAboutValuesInPlainWords(t *testing.T) {
	st := groundingState("Enter the purchase order from the supplier.")
	_, untraced := groundingCheck(st, `gui type e9 "Green Furnishings"`)
	q := untracedQuestion(untraced, "Not run: ...")
	if !strings.Contains(q, `I was about to type "Green Furnishings"`) || strings.Contains(q, "appear nowhere") ||
		strings.Contains(q, "Not run") || !strings.Contains(q, "what should I type instead?") {
		t.Errorf("question = %q", q)
	}
}

func TestTheOperatorIsToldOnceWhenMostOfItsTimeIsGone(t *testing.T) {
	began := time.Date(2026, 10, 19, 6, 20, 0, 0, time.UTC)
	deadline := began.Add(25 * time.Minute)
	st := &operatorState{Commands: []string{
		"gui open /home/headless/work/inbox/price_list.xlsx",
		`gui type "Item"; gui key tab; gui type "Qty"; gui key enter`,
		"while read v; do gui type \"$v\"; gui key enter; done < cells.tsv",
		"office read /home/headless/work/inbox/price_list.xlsx",
	}}
	if note := timeNoteFor(st, began, deadline, began.Add(10*time.Minute)); note != "" {
		t.Fatalf("told with most of the time left: %q", note)
	}
	note := timeNoteFor(st, began, deadline, began.Add(16*time.Minute))
	for _, want := range []string{"about 9 of this task's 25 minutes are left", "6 screen actions so far, about 160 seconds each",
		"rows of values go into a sheet with one gui type", "save it", "HELP"} {
		if !strings.Contains(note, want) {
			t.Errorf("note lacks %q: %q", want, note)
		}
	}
	if note := timeNoteFor(st, began, began.Add(3*time.Minute), began.Add(150*time.Second)); note != "" {
		t.Errorf("a goal of a few minutes was told: %q", note)
	}
	if note := timeNoteFor(&operatorState{}, began, deadline, began.Add(20*time.Minute)); strings.Contains(note, "screen actions") {
		t.Errorf("no screen actions, yet counted: %q", note)
	}
}

func TestAKilledCommandSaysItDidNotFinish(t *testing.T) {
	res := runShell(context.Background(), t.TempDir(), "sleep 5", 200*time.Millisecond)
	if res.ExitCode != 124 || !strings.Contains(res.Output, "It did NOT finish") || !strings.Contains(res.Output, "Look at the screen or the file") {
		t.Errorf("a killed command: exit %d, %q", res.ExitCode, res.Output)
	}
}

func TestAShareDesktopTellsTheOperatorOneLineAboutIt(t *testing.T) {
	t.Setenv("DECURIX_SHARED_FOLDER", `C:\Users\Administrator\Documents\Decurix demo`)
	rule := sharedRule(sharedFolderHost())
	for _, want := range []string{
		`C:\Users\Administrator\Documents\Decurix demo on their computer`,
		`C:\Users\Administrator\Documents\Decurix demo\x.xlsx is /home/headless/PC/x.xlsx`,
		"your own tools (office, python)", "never by typing them into one of their apps a key at a time",
		"the named-app and source-file rules above say how", "Nothing else of their computer is reachable",
	} {
		if !strings.Contains(rule, want) {
			t.Errorf("the rule lacks %q:\n%s", want, rule)
		}
	}
	if strings.Count(rule, "\n") != 1 {
		t.Errorf("the rule is one line: %q", rule)
	}
	// A POSIX folder is told the way it is written.
	if !strings.Contains(sharedRule("/Users/ann/Shared"), "/Users/ann/Shared/x.xlsx is /home/headless/PC/x.xlsx") {
		t.Error("a POSIX folder's example path")
	}
	// No product names in a line every desktop with a shared folder reads.
	for _, name := range []string{"Excel", "Word", "PowerPoint", "Calc", "Notepad", "Paint"} {
		if strings.Contains(rule, name) {
			t.Errorf("the rule names %s", name)
		}
	}
	// A desktop without one is told nothing.
	t.Setenv("DECURIX_SHARED_FOLDER", "")
	if sharedFolderHost() != "" {
		t.Error("a desktop with no shared folder has one")
	}
}

func TestTheAppTheRequestNamesHasToOpenTheFileMadeInTheSharedFolder(t *testing.T) {
	t.Setenv("DECURIX_SHARED_FOLDER", `C:\Users\Administrator\Documents\Decurix demo`)
	named := "Use Excel (the app on your desktop) to open orders_oct.xlsx from the Decurix demo folder and add a Unit Price column. Save it as orders_oct_priced.xlsx."
	plain := "Open orders_oct.xlsx from the Decurix demo folder and add a Unit Price column. Save it as orders_oct_priced.xlsx."
	made := "office update /home/headless/PC/orders_oct_priced.xlsx - <<'EOF'\n{}\nEOF"

	st := &operatorState{Request: named, Worked: []string{"office read /home/headless/PC/orders_oct.xlsx", made}}
	nudge := sharedAppUntouchedNudge(st)
	if !strings.Contains(nudge, "no window of excel was opened") || !strings.Contains(nudge, `C:\Users\Administrator\Documents\Decurix demo`) {
		t.Fatalf("a file made with tools for a request that names an app: %q", nudge)
	}
	// Once the app was worked, what it shows is judged as ever.
	// Another app opened in its place is not it.
	st.Worked = append(st.Worked, "gui open spreadsheet")
	if namedAppWindow(named, "gui open spreadsheet", `open spreadsheet - Opened "Untitled 1 - LibreOffice Calc".`) {
		t.Error("another spreadsheet app was taken for the one named")
	}
	if n := sharedAppUntouchedNudge(st); n == "" {
		t.Error("another app was taken for the one named")
	}
	// A window of the named app, however it was reached: opened, or worked in.
	for _, out := range []string{`open Excel (Host) - Opened "Excel".`, `click button "Save" (e12) in "orders_oct_priced - Excel - Chromium"`,
		`Screen 1280x720. Active window: "Book1 - Excel"`} {
		if !namedAppWindow(named, `gui open "Excel (Host)"`, out) {
			t.Errorf("not seen as the named app: %s", out)
		}
	}
	if namedAppWindow(named, "office read x.xlsx", `Opened "Excel"`) {
		t.Error("a command that is not gui was taken for the app")
	}
	st.NamedAppOpened = true
	if n := sharedAppUntouchedNudge(st); n != "" {
		t.Errorf("after the app was opened: %q", n)
	}
	// A request that names no app: nothing to say. One that names it: the app must open the file, wherever it was made
	// and with or without a shared folder (Finding BQ: the tools make the content, the app handles it).
	if n := sharedAppUntouchedNudge(&operatorState{Request: plain, Worked: []string{made}}); n != "" {
		t.Errorf("a request that names no app: %q", n)
	}
	if n := sharedAppUntouchedNudge(&operatorState{Request: named, Worked: []string{"office update /home/headless/work/x.xlsx - <<'EOF'\n{}\nEOF"}}); n == "" {
		t.Error("a file made with tools outside the shared folder was never opened in the named app, and was taken")
	}
	t.Setenv("DECURIX_SHARED_FOLDER", "")
	if n := sharedAppUntouchedNudge(&operatorState{Request: named, Worked: []string{made}}); !strings.Contains(n, "gui open FILE") {
		t.Errorf("no shared folder: %q", n)
	}
}

func TestATypedPathUnderTheSharedFolderIsTraced(t *testing.T) {
	st := groundingState(`Save the result as orders_oct_priced.xlsx in the Decurix demo folder in my Documents (C:\Users\Administrator\Documents\Decurix demo).`)
	if n := groundingNudge(st, `gui type e9 "/home/headless/PC/orders_oct_priced.xlsx"`); n != "" {
		t.Errorf("the shared folder's own place was refused: %q", n)
	}
	if n := groundingNudge(st, `gui type e9 "/home/headless/PC/something_else_entirely.xlsx"`); n == "" {
		t.Error("a file the request never named was let through")
	}
}

// The live case (2026-10-19, word_letters on a shared-folder worker with no host apps): the named app could not be
// opened, another word processor was, the letters were typed there and saved into the shared folder - and nothing said
// the named app was never used.
func TestWorkDoneInAnotherAppThanTheOneNamedIsSentBackAndThenAsked(t *testing.T) {
	t.Setenv("DECURIX_SHARED_FOLDER", `C:\Users\Administrator\Documents\Decurix demo`)
	st := &operatorState{Request: `From receivables.xlsx in the "Decurix demo" folder in my Documents (C:\Users\Administrator\Documents\Decurix demo), write payment reminder letters in Word (the Word app on your desktop): one letter per customer. Save them as one document reminders.docx in the same folder.`}
	steps := []struct{ command, output string }{
		{"office read /home/headless/PC/receivables.xlsx", "receivables.xlsx: sheets ['Invoices']"},
		{"gui open word", `open word - Opened "Untitled 1 - LibreOffice Writer".`},
		{`gui type "15 October 2026"`, `type "15 October 2026" into the screen - paragraph changed`},
		{`gui click e242`, `click button "Save" (e242) in "Save" - window "Confirm File Format" opened`},
	}
	for _, step := range steps {
		st.Worked = append(st.Worked, step.command)
		if namedAppWindow(st.Request, step.command, step.output) {
			st.NamedAppOpened = true
		}
	}
	if st.NamedAppOpened {
		t.Fatal("another word processor was taken for the app the request names")
	}
	nudge := sharedAppUntouchedNudge(st)
	for _, want := range []string{"no window of word was opened", "another app", "do not use another app in its place: reply HELP"} {
		if !strings.Contains(nudge, want) {
			t.Errorf("nudge lacks %q: %s", want, nudge)
		}
	}
	// No product name is hard-coded: the app is the request's own word.
	if !strings.Contains(sharedAppUntouchedNudge(&operatorState{Request: "Use Impress to make the deck in the shared folder.", Worked: []string{"gui open presentation"}}), "done in impress") {
		t.Error("the app named is not the request's own")
	}
}
