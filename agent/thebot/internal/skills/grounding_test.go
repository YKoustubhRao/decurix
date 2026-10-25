package skills

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"thebot/internal/llm"
)

// groundingNudge is groundingCheck's nudge alone.
func groundingNudge(st *operatorState, command string) string {
	nudge, _ := groundingCheck(st, command)
	return nudge
}

func TestTypedValuesReadsWhatACommandWouldType(t *testing.T) {
	cases := map[string][]string{
		`gui do enter-po --param customer="Green Furnishings" --param quantity="2; 12"`: {"Green Furnishings", "2", "12"},
		`gui do enter-po --param ref=GF-5522 --from 3`:                                  {"GF-5522"},
		`gui type e12 "Gemini Furniture"`:                                               {"Gemini Furniture"},
		`gui type "hello there"`:                                                        {"hello there"},
		`gui type --append e3 "more"`:                                                   {"more"},
		`gui select e4 "Main"`:                                                          {"Main"},
		`gui click "Save quotation"`:                                                    nil,
		`office read /x/po.pdf`:                                                         nil,
		`echo gui do x --param a=b`:                                                     nil,
	}
	for cmd, want := range cases {
		if got := typedValues(cmd); !reflect.DeepEqual(got, want) {
			t.Errorf("typedValues(%q) = %q, want %q", cmd, got, want)
		}
	}
}

func groundingState(request string, commands ...string) *operatorState {
	st := &operatorState{Request: request}
	st.noteSeen(request)
	st.Commands = commands
	return st
}

// The live case: a document named in the request, never read.
func TestGroundingRefusesToTypeBeforeTheDocumentIsRead(t *testing.T) {
	st := groundingState("A customer sent us a purchase order: /home/headless/work/inbox/a/GF-5522.pdf. Enter it.")
	nudge := groundingNudge(st, `gui do enter-po --param customer="Green Furnishings" --param ref="GF-5522"`)
	if !strings.Contains(nudge, "have not read it") || !strings.Contains(nudge, "office read /home/headless/work/inbox/a/GF-5522.pdf") {
		t.Fatalf("nudge = %q", nudge)
	}
}

// Read, but a value in no output: still a guess.
func TestGroundingRefusesAValueNothingShowed(t *testing.T) {
	st := groundingState("Enter /w/GF-5522.pdf as a quotation.", "office read /w/GF-5522.pdf")
	st.noteSeen("Customer: Gemini Furniture\nPO: GF-5522\nCabinet with Doors  3")

	if n := groundingNudge(st, `gui do enter-po --param customer="Green Furnishings"`); !strings.Contains(n, `"Green Furnishings"`) {
		t.Fatalf("an invented customer was let through: %q", n)
	}
	// Found in the document, however it was spaced or capitalised.
	if n := groundingNudge(st, `gui do enter-po --param customer="gemini  furniture" --param ref=GF-5522 --param q=3`); n != "" {
		t.Fatalf("a value from the document was refused: %q", n)
	}
}

// A file the request names, saved in the work folder or a folder it names, is a path the task gave.
func TestGroundingTakesAFilePathTheRequestGave(t *testing.T) {
	st := groundingState("Make a deck from the brief. Save it as q3_review.pptx in my work folder.")
	for _, ok := range []string{
		`gui type e12 "/home/headless/work/q3_review.pptx"`,
		`gui type e12 "~/work/out/q3_review.pptx"`,
	} {
		if n := groundingNudge(st, ok); n != "" {
			t.Fatalf("%s was refused: %q", ok, n)
		}
	}
	st = groundingState(`Save it as report.xlsx in C:\Users\Administrator\Documents\Decurix demo.`)
	if n := groundingNudge(st, `gui type e12 "C:\Users\Administrator\Documents\Decurix demo\report.xlsx"`); n != "" {
		t.Fatalf("a folder the request named was refused: %q", n)
	}
	for _, bad := range []string{
		`gui type e12 "/home/headless/work/other_name.pptx"`, // a name nobody gave
		`gui type e12 "/etc/q3_review.pptx"`,                 // a folder nobody gave
	} {
		if n := groundingNudge(groundingState("Save it as q3_review.pptx in my work folder."), bad); n == "" {
			t.Fatalf("%s was let through", bad)
		}
	}
}

// The file type for a name the request gave is chosen without asking; another type is still a guess.
func TestGroundingTakesTheFileTypeOfANameTheRequestGave(t *testing.T) {
	st := groundingState("Save it as sales_q3_report.xlsx in my work folder.")
	if n := groundingNudge(st, `gui select e40 "Excel Workbook (*.xlsx)"`); n != "" {
		t.Fatalf("the requested file type was refused: %q", n)
	}
	if n := groundingNudge(st, `gui select e40 "Excel Macro-Enabled Workbook (*.xlsm)"`); n == "" {
		t.Fatal("a file type nobody asked for was let through")
	}
}

// A cell or range typed to go there is a place, not a value.
func TestGroundingTakesACellReferenceAsAPlace(t *testing.T) {
	st := groundingState("Add a Total row under the data.")
	for _, ok := range []string{`gui type e3 "A20"`, `gui type e3 "A1:D1"`, `gui type e3 "$D$2"`} {
		if n := groundingNudge(st, ok); n != "" {
			t.Fatalf("%s was refused: %q", ok, n)
		}
	}
}

// A letterhead in capitals is typed as the running text writes it.
func TestGroundingAsksForTheDocumentsOwnSpelling(t *testing.T) {
	st := groundingState("Enter /w/GF-5522.pdf.", "office read /w/GF-5522.pdf")
	st.noteSeen("GEMINI FURNITURE\nPURCHASE ORDER\nPO Number: GF-5522\nAuthorised by Purchasing, Gemini Furniture")

	n := groundingNudge(st, `gui do enter-po --param customer="GEMINI FURNITURE" --param ref=GF-5522`)
	if !strings.Contains(n, `"GEMINI FURNITURE" as "Gemini Furniture"`) {
		t.Fatalf("nudge = %q", n)
	}
	for _, cmd := range []string{
		`gui do enter-po --param customer="Gemini Furniture" --param ref=GF-5522`, // as written
		`gui type e1 "PURCHASE ORDER"`,                                            // only ever in capitals
	} {
		if n := groundingNudge(st, cmd); n != "" {
			t.Errorf("%s: %q", cmd, n)
		}
	}
}

// A scan shown as a picture has no text to search: only the read is checked.
func TestGroundingTrustsWhatCameAsAPicture(t *testing.T) {
	st := groundingState("Enter /w/scan.pdf.", "office read /w/scan.pdf")
	st.ShownPages = true
	if n := groundingNudge(st, `gui type e1 "Gemini Furniture"`); n != "" {
		t.Fatalf("nudge = %q", n)
	}
}

// Commands that type nothing are never held up.
func TestGroundingIgnoresCommandsThatTypeNothing(t *testing.T) {
	st := groundingState("Enter /w/po.pdf.")
	for _, cmd := range []string{`gui click "New quotation"`, `office read /w/po.pdf`, `ls ~/work`} {
		if n := groundingNudge(st, cmd); n != "" {
			t.Errorf("%s: %q", cmd, n)
		}
	}
}

// Found live on a native Windows worker: `gui open notepad`, then "Notepad
// opened and Hello World typed" - nothing had typed anything.
func TestADoneForTypingNoCommandDidIsPushedBack(t *testing.T) {
	st := func(request string, commands ...string) *operatorState {
		return &operatorState{Request: request, Commands: commands, Worked: commands}
	}
	// A gui type that failed (aimed at a button) typed nothing.
	refused := &operatorState{Request: "open notepad and type hello into it",
		Commands: []string{"gui open notepad", `gui type e40 "hello"`}, Worked: []string{"gui open notepad"}}
	if got := unperformedScreenNudge(refused); !strings.Contains(got, "no command has typed") {
		t.Errorf("a refused gui type counted as typing: %q", got)
	}
	for _, c := range []struct {
		name string
		st   *operatorState
		want string
	}{
		{"opened only", st("open notepad and type hello world into it", "gui open notepad"), "no command has typed"},
		{"typed", st("open notepad and type hello world into it", "gui open notepad", `gui type e1 "hello world"`), ""},
		{"chained", st("open notepad and type hello into it", `gui open notepad; gui type e1 "hello"`), ""},
		{"a click asked for", st("open the form and click Save", "gui open form.html", "gui observe"), "click or press"},
		{"clicked", st("open the form and click Save", "gui open form.html", `gui click "Save"`), ""},
		{"a taught skill may have done it", st("type the order into the app", "gui do enter-order --param ref=A1"), ""},
		{"not screen work", st("type a note into notes.txt", `Set-Content notes.txt "x"`), ""},
		{"quoted words are not the task", st(`open notepad and show me "type here"`, "gui open notepad"), ""},
		{"press Enter is not typing", st("open the form and press Enter", "gui open form.html", "gui key Return"), ""},
	} {
		got := unperformedScreenNudge(c.st)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: nudge = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestTheFieldHintNamesWhereTypingGoes(t *testing.T) {
	st := &operatorState{Request: "open notepad and type hello world into it"}
	listing := "Screen 1920x1080. Active window: \"Untitled - Notepad\"\n" +
		"e28 document \"Text editor\"\ne40 button \"Strikethrough (Ctrl+Shift+X)\" [focused]\ne10 textbox \"Go to\" [disabled]\n"
	hint := typingFieldHint(st, "gui observe", listing)
	if !strings.Contains(hint, `e28 document "Text editor"`) || !strings.Contains(hint, "gui type e28") ||
		strings.Contains(hint, "e40") || strings.Contains(hint, "e10") {
		t.Errorf("hint = %q", hint)
	}
	if hint := typingFieldHint(st, "gui click e40", listing); hint != "" {
		t.Errorf("a hint followed a command that is not an observe: %q", hint)
	}
	if hint := typingFieldHint(&operatorState{Request: "open notepad"}, "gui observe", listing); hint != "" {
		t.Errorf("a hint for a task that types nothing: %q", hint)
	}
}

// Through the loop: pushed back once, and a second "done" over nothing typed
// goes to the person as a question - never out as a result. Their "done"
// lets it finish; "nothing was typed" does not.
func TestOperatorAsksAPersonRatherThanReportTypingItNeverDid(t *testing.T) {
	stubTools(t, "")
	chat := &scriptedChat{replies: []string{
		"COMMAND:\ngui open notepad",
		"DONE:\nNotepad opened and Hello World typed.",
		"DONE:\nNotepad was opened and Hello World was typed into it.",
		"DONE:\nStill done.",
		"DONE:\nOpened Notepad; you confirmed the text is there.",
	}}
	op := NewOperator()
	rc := operatorContext(t, chat)
	task := Subtask{Step: "open notepad and type hello world into it", Type: "operator"}

	res, _ := op.Run(rc, task)
	if res.Verified || res.Meta["needsUserInput"] != true || !strings.Contains(res.Output, "cannot confirm") || res.Checkpoint == nil {
		t.Fatalf("result = %+v, want a question for the person", res)
	}
	if nudge := chat.calls[2][len(chat.calls[2])-1].Content; !strings.Contains(nudge, "no command has typed") {
		t.Errorf("the first done was not pushed back: %q", nudge)
	}

	// "No" is not a confirmation: the next done is asked about again.
	rc.Resume, rc.HumanReply = res.Checkpoint, "nothing was typed"
	res, _ = op.Run(rc, task)
	if res.Verified || !strings.Contains(res.Output, "cannot confirm") {
		t.Fatalf("after a no, result = %+v, want the question again", res)
	}

	rc.Resume, rc.HumanReply = res.Checkpoint, "done"
	res, _ = op.Run(rc, task)
	if !res.Verified || !strings.Contains(res.Output, "confirmed") {
		t.Fatalf("after the person's done, result = %+v", res)
	}
}

// stubTools puts stand-in gui and office commands first on PATH; gui logs
// every call so the test can see what reached the app.
func stubTools(t *testing.T, officeOutput string) (log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "gui.log")
	said := filepath.Join(dir, "office.txt")
	if err := os.WriteFile(said, []byte(officeOutput+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stubWorkerTools(t,
		`printf '%s\n' "$*" >> `+log+"\necho done\n",
		"cat "+said+"\n",
		"if \"%2\"==\"decurix_agent.tools.office\" goto office\r\n"+
			">>\""+log+"\" echo %DECURIX_CLI_ARGS%\r\necho done\r\nexit /b 0\r\n"+
			":office\r\ntype \""+said+"\"\r\n")
	return log
}

// End to end through the loop: the invented values never reach the app, the
// document is read, and the values it holds are typed.
func TestOperatorReadsTheDocumentBeforeTypingItsValues(t *testing.T) {
	log := stubTools(t, "Customer: Gemini Furniture\nPO: GF-5522")
	chat := &scriptedChat{replies: []string{
		`COMMAND:` + "\n" + `gui do enter-po --param customer="Green Furnishings" --param ref="GF-5522"`,
		"COMMAND:\noffice read /w/GF-5522.pdf",
		`COMMAND:` + "\n" + `gui do enter-po --param customer="Gemini Furniture" --param ref="GF-5522"`,
		"DONE:\nEntered GF-5522 for Gemini Furniture.",
	}}
	res, err := NewOperator().Run(operatorContext(t, chat), Subtask{Step: "A customer sent a PO: /w/GF-5522.pdf. Enter it.", Type: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Verified {
		t.Fatalf("result = %+v", res)
	}
	got, _ := os.ReadFile(log)
	if strings.Contains(string(got), "Green Furnishings") || !strings.Contains(string(got), "Gemini Furniture") {
		t.Fatalf("what reached the app:\n%s", got)
	}
	if turn2 := chat.calls[1][len(chat.calls[1])-1].Content; !strings.Contains(turn2, "have not read it") {
		t.Errorf("the refusal did not say why:\n%s", turn2)
	}
}

// Sent back twice and still guessing: the person is asked, nothing is typed.
func TestOperatorAsksWhenItKeepsTypingWhatItCannotTrace(t *testing.T) {
	log := stubTools(t, "Customer: Gemini Furniture")
	guess := `COMMAND:` + "\n" + `gui do enter-po --param customer="Green Furnishings"`
	chat := &scriptedChat{replies: []string{"COMMAND:\noffice read /w/po.pdf", guess,
		`COMMAND:` + "\n" + `gui do enter-po --param customer="Green Furniture"`,
		`COMMAND:` + "\n" + `gui do enter-po --param customer="Greene Furniture"`}}
	res, err := NewOperator().Run(operatorContext(t, chat), Subtask{Step: "Enter /w/po.pdf.", Type: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Verified || res.Meta["needsUserInput"] != true || res.Checkpoint == nil {
		t.Fatalf("result = %+v", res)
	}
	if got, _ := os.ReadFile(log); len(got) > 0 {
		t.Fatalf("something reached the app:\n%s", got)
	}
}

// Found live: the answer to the request before was sent again as this one's.
func TestOperatorDoesNotRepeatAnEarlierAnswerAsThisOne(t *testing.T) {
	earlier := "Entered the PO as a quotation in the sales app. The page shows: Saved quotation Q-016 for Gemini Furniture."
	chat := &scriptedChat{replies: []string{
		"DONE:\n" + earlier,
		"COMMAND:\necho reviewed",
		"DONE:\nWrote the price review.",
	}}
	rc := operatorContext(t, chat)
	rc.SessionHistory = []llm.Message{
		{Role: "user", Content: "Enter the PO in our sales app."},
		{Role: "assistant", Content: earlier},
	}
	res, err := NewOperator().Run(rc, Subtask{Step: "Review the supplier's new prices.", Type: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "Wrote the price review") {
		t.Fatalf("result = %+v", res)
	}
	if turn2 := chat.calls[1][len(chat.calls[1])-1].Content; !strings.Contains(turn2, "EARLIER request") {
		t.Errorf("the push back did not say why:\n%s", turn2)
	}
}

// A request that needs no command still ends on its first DONE, and a
// document task answered unread is sent back once.
func TestEarlyDoneNudgeIsNarrow(t *testing.T) {
	if n := earlyDoneNudge(groundingState("ping - just reply pong"), nil, "pong"); n != "" {
		t.Errorf("pong: %q", n)
	}
	if n := earlyDoneNudge(groundingState("Summarise /w/report.pdf"), nil, "It is about sales."); !strings.Contains(n, "office read /w/report.pdf") {
		t.Errorf("unread document: %q", n)
	}
	history := []llm.Message{{Role: "assistant", Content: "pong"}}
	if n := earlyDoneNudge(groundingState("ping again"), history, "pong"); n == "" {
		t.Errorf("a short answer said again is still an echo when it is the whole earlier answer")
	}
}

// A file a person attached is a document of the request whatever its kind,
// and an answer given before anything read it is sent back.
func TestAnAttachedFileIsADocumentOfTheRequest(t *testing.T) {
	request := "what does this say?\n\n" + attachedFilesMark +
		"\n  /home/headless/work/inbox/up-1/notes.txt\n  /home/headless/work/inbox/up-1/PO.pdf"

	docs := requestDocuments(request)
	if len(docs) != 2 || docs[0] != "/home/headless/work/inbox/up-1/PO.pdf" || docs[1] != "/home/headless/work/inbox/up-1/notes.txt" {
		t.Fatalf("documents = %q", docs)
	}

	st := groundingState("what does this say?\n\n" + attachedFilesMark + "\n  /home/headless/work/inbox/up-1/notes.txt")
	if n := earlyDoneNudge(st, nil, "It is a shopping list."); !strings.Contains(n, "office read /home/headless/work/inbox/up-1/notes.txt") {
		t.Errorf("an unread attachment was answered about: %q", n)
	}

	st.Commands = []string{"office read /home/headless/work/inbox/up-1/notes.txt"}
	if n := earlyDoneNudge(st, nil, "It is a shopping list."); n != "" {
		t.Errorf("an answer after reading the attachment was pushed back: %q", n)
	}
}

// A bill writes its figures with separators and paise; the form takes the plain number. The same number in another
// spelling is traced; a number the bill does not hold is still a guess.
func TestGroundingKnowsANumberHoweverItIsWritten(t *testing.T) {
	st := groundingState("Enter /w/BILL_03.pdf in the project portal.", "office read /w/BILL_03.pdf")
	st.noteSeen("Mesh Chairs  Qty: 2 Chairs | Rate: Rs. 6,000.00 | Amount: Rs. 12,000.00\nGRAND TOTAL: Rs. 1,88,800.00")

	for _, cmd := range []string{`gui type e892 "6000"`, `gui type e892 "6000.00"`, `gui type e892 "₹6,000"`, `gui type e1 "188800"`} {
		if n := groundingNudge(st, cmd); n != "" {
			t.Errorf("%s was refused: %q", cmd, n)
		}
	}
	if n := groundingNudge(st, `gui type e892 "7000"`); !strings.Contains(n, `"7000"`) {
		t.Errorf("a figure the bill does not hold was let through: %q", n)
	}
	if n := groundingNudge(st, `gui type e5 "6000 chairs"`); !strings.Contains(n, `"6000 chairs"`) {
		t.Errorf("text that is not only a number was matched as one: %q", n)
	}
}

func TestGroundingKnowsADateHoweverItIsWritten(t *testing.T) {
	st := groundingState("Enter /w/BILL_04.pdf in the project portal.", "office read /w/BILL_04.pdf")
	st.noteSeen("Invoice No: SC/TAX/26-27/088\nInvoice Date: 04-Oct-2026\nDue: 3 November 2026")

	for _, cmd := range []string{`gui type e1 "04-10-2026"`, `gui type e1 "2026-10-04"`, `gui type e1 "4/10/2026"`,
		`gui type e1 "03-11-2026"`, `gui type e1 "11/03/2026"`} {
		if n := groundingNudge(st, cmd); n != "" {
			t.Errorf("%s was refused: %q", cmd, n)
		}
	}
	if n := groundingNudge(st, `gui type e1 "05-10-2026"`); !strings.Contains(n, `"05-10-2026"`) {
		t.Errorf("a date the bill does not hold was let through: %q", n)
	}
	if n := groundingNudge(st, `gui type e1 "04-10-2026 paid"`); !strings.Contains(n, `"04-10-2026 paid"`) {
		t.Errorf("text that is more than a date was matched as one: %q", n)
	}
}

func TestGroundingKnowsAnyCurrencyMark(t *testing.T) {
	st := groundingState("Enter /w/I.pdf.", "office read /w/I.pdf")
	st.noteSeen("Total due: EUR 1,250.50  Fee: £75.00  Net: USD 3,000")
	for _, cmd := range []string{`gui type e1 "1250.5"`, `gui type e1 "€1,250.50"`, `gui type e1 "75"`, `gui type e1 "$3000"`} {
		if n := groundingNudge(st, cmd); n != "" {
			t.Errorf("%s was refused: %q", cmd, n)
		}
	}
	if n := groundingNudge(st, `gui type e1 "1250.6"`); !strings.Contains(n, `"1250.6"`) {
		t.Errorf("a figure the document does not hold was let through: %q", n)
	}
}

func TestGroundingDoesNotLetADateBeSwappedAcrossDayAndMonth(t *testing.T) {
	// A document that writes 04-10-2026 in digits does not say which field is the month.
	st := groundingState("Enter /w/I.pdf.", "office read /w/I.pdf")
	st.noteSeen("Issued: 04-10-2026")
	for _, cmd := range []string{`gui type e1 "04-10-2026"`, `gui type e1 "4/10/2026"`, `gui type e1 "04.10.2026"`} {
		if n := groundingNudge(st, cmd); n != "" {
			t.Errorf("%s (same order as the document) was refused: %q", cmd, n)
		}
	}
	for _, cmd := range []string{`gui type e1 "10-04-2026"`, `gui type e1 "2026-10-04"`, `gui type e1 "2026-04-10"`} {
		if n := groundingNudge(st, cmd); n == "" {
			t.Errorf("%s was let through although the document's own order is not known", cmd)
		}
	}

	// One that cannot be misread (a day past 12, or the month named) is converted freely.
	st = groundingState("Enter /w/I.pdf.", "office read /w/I.pdf")
	st.noteSeen("Issued: 25-10-2026   Due: 3 Nov 2026")
	for _, cmd := range []string{`gui type e1 "2026-10-25"`, `gui type e1 "10/25/2026"`, `gui type e1 "25.10.2026"`,
		`gui type e1 "03-11-2026"`, `gui type e1 "11/03/2026"`} {
		if n := groundingNudge(st, cmd); n != "" {
			t.Errorf("%s was refused: %q", cmd, n)
		}
	}
	if n := groundingNudge(st, `gui type e1 "2026-25-10"`); n == "" {
		t.Errorf("an impossible date was let through")
	}
}

// A formula is the worker's own construction: not looked for in what was read,
// though a figure inside it still is.
func TestGroundingLetsAFormulaThrough(t *testing.T) {
	st := groundingState("Put the total of the Q1 column (B2 to B5) in cell B6 using a formula, then save.")
	st.noteSeen("Rent 1200\nPayroll 5400")

	for _, ok := range []string{`gui type e30 "=SUM(B2:B5)"`, `gui type e30 "=SUM(B2:B5)\n"`, `gui type "=B2+B3*$C$1"`, `gui type e3 "=B2+1200"`} {
		if n := groundingNudge(st, ok); n != "" {
			t.Errorf("%s was refused: %q", ok, n)
		}
	}
	for _, bad := range []string{`gui type e30 "=B2+9999"`, `gui type e30 "=\"Acme Ltd\""`, `gui type e30 "=cmd|' /C calc'!A0"`} {
		if n := groundingNudge(st, bad); n == "" {
			t.Errorf("%s was let through", bad)
		}
	}
}

// A formula built from what the request says is the worker's own construction, quoted words and all. Found live
// (2026-10-19): every =IF(...="maharashtra",E2*9%,0) was refused as typing a value never read.
func TestGroundingLetsAFormulaWithTheRequestsWordsThrough(t *testing.T) {
	st := groundingState("Use the spreadsheet app to add CGST and SGST: a sale to a customer in Maharashtra gets CGST 9% and SGST 9% of the taxable value, any other state IGST 18%.")
	st.noteSeen("A2='INV-1'  D2='Maharashtra '  E2=12400")
	for _, ok := range []string{
		`gui type e30 "=IF(LOWER(TRIM(D2))=\"maharashtra\",E2*9%,0)"`,
		`gui type e30 "=IF(LOWER(TRIM(D2))=\"maharashtra\",0,E2*18%)"`,
		`gui type e30 "=IF(D2=\"\",\"\",E2*9%)"`,
		`gui type e30 "=IF(TRIM(D2)=\"Maharashtra\",E2*9%,0)\t=E2+F2"`,
	} {
		if n := groundingNudge(st, ok); n != "" {
			t.Errorf("%s was refused: %q", ok, n)
		}
	}
	for _, bad := range []string{
		`gui type e30 "=IF(D2=\"Gujarat\",E2*9%,0)"`,
		`gui type e30 "=IF(D2=\"maharashtra\",E2*12%,0)"`,
		`gui type e30 "=E2*0.07"`,
	} {
		if n := groundingNudge(st, bad); n == "" {
			t.Errorf("%s was let through", bad)
		}
	}
}

// Work in a window cannot be reported done before a single command has run.
func TestScreenWorkIsNotDoneBeforeAnythingRan(t *testing.T) {
	st := groundingState("Rename the sheet in the spreadsheet open in LibreOffice Calc from Sheet1 to Budget 2026, then save the file.")
	if n := unstartedScreenNudge(st); !strings.Contains(n, "nothing has been run") || !strings.Contains(n, "gui observe") {
		t.Fatalf("a done with nothing run was taken: %q", n)
	}
	st.CommandsRun = 1
	if n := unstartedScreenNudge(st); n != "" {
		t.Fatalf("pushed back after a command ran: %q", n)
	}
	// A request that needs no window is answered as before.
	if n := unstartedScreenNudge(groundingState("ping - just reply pong")); n != "" {
		t.Fatalf("a plain answer was refused: %q", n)
	}
}

// Nor can work on a file the request names. Found live: "summary_feb.xlsx is
// complete ... I verified the workbook by reading it back", three seconds,
// no command, no file, the figures invented.
func TestFileWorkIsNotDoneBeforeAnythingRan(t *testing.T) {
	st := groundingState("Summarize sales_feb.csv from the work folder into the workbook summary_feb.xlsx: one row per region with its total revenue")
	n := unstartedScreenNudge(st)
	if !strings.Contains(n, "no file has been read, written or changed") || strings.Contains(n, "gui observe") {
		t.Fatalf("a done with nothing run was taken, or sent to the screen: %q", n)
	}
	st.CommandsRun = 1
	if n := unstartedScreenNudge(st); n != "" {
		t.Fatalf("pushed back after a command ran: %q", n)
	}

	for _, work := range []string{
		"append INV-7001 to ~/work/invoices.txt",
		`Create "totals.xlsx" with a Total row`,
		"convert report.docx into a PDF",
	} {
		if unstartedScreenNudge(groundingState(work)) == "" {
			t.Errorf("%q was taken as done with nothing run", work)
		}
	}
	// A question that names a file, or a kind of file, asks for nothing to be done to one.
	for _, talk := range []string{
		"what is a .csv file?",
		"is notes.txt a good name for it?",
		"hello, whats your desktop name?",
		"what does e.g. mean",
	} {
		if n := unstartedScreenNudge(groundingState(talk)); n != "" {
			t.Errorf("%q was refused: %q", talk, n)
		}
	}
}

// Typing into the application's own search box finds a page; it records nothing (found live, 2026-10-13).
func TestGroundingLetsTheOperatorSearchTheApplication(t *testing.T) {
	st := groundingState("Record this customer payment: /w/REMIT_01.pdf", "office read /w/REMIT_01.pdf")
	st.noteSeen("Remittance advice WVS/RA/0457 ... 229,000.00")
	st.noteSeen("e9071 combobox \"Search\" [focused]\ne9100 textbox \"Reference No\"")

	if n := groundingNudge(st, `gui type e9071 "Payment Entry"`); n != "" {
		t.Fatalf("a search for a page was refused: %q", n)
	}
	// A field that keeps what is typed is guarded as before.
	if n := groundingNudge(st, `gui type e9100 "Payment Entry"`); !strings.Contains(n, `"Payment Entry"`) {
		t.Fatalf("an untraced value typed into a field was let through: %q", n)
	}
	// Named as an action's result names it, too.
	st.noteSeen(`type "x" into searchbox "Find" (e9200)`)
	if n := groundingNudge(st, `gui type e9200 "Supplier List"`); n != "" {
		t.Fatalf("a search box named in an action's result was not recognised: %q", n)
	}
}

// Found live (2026-10-14): a phone photo of a clear invoice was called illegible by a worker that only opened it with
// a Python script and never looked at it.
func TestAPictureMustBeLookedAtBeforeTheWorkIsDone(t *testing.T) {
	req := "Run today's intake.\n\nFiles attached to this request, saved on this desktop:\n  /home/headless/work/inbox/up-1/IMG_4471.png\n  /home/headless/work/inbox/up-1/notes.txt"
	st := groundingState(req, "python3 - <<'PY'\nfrom PIL import Image\nImage.open('/home/headless/work/inbox/up-1/IMG_4471.png')\nPY")
	unseen := unseenPictures(st)
	if len(unseen) != 1 || !strings.HasSuffix(unseen[0], "IMG_4471.png") {
		t.Fatalf("unseen = %v", unseen)
	}
	if n := unseenPicturesNudge(unseen); !strings.Contains(n, "office read /home/headless/work/inbox/up-1/IMG_4471.png") {
		t.Fatalf("nudge = %q", n)
	}
	st.Commands = append(st.Commands, "office read /home/headless/work/inbox/up-1/IMG_4471.png")
	if unseen := unseenPictures(st); len(unseen) != 0 {
		t.Fatalf("after office read, unseen = %v", unseen)
	}
}

// Found live (2026-10-14): a Thai spam phrase typed into a customer field paged a person after two refusals.
func TestTextInAScriptTheTaskNeverUsesIsASlip(t *testing.T) {
	st := groundingState("Enter the orders from /w/order_batch_A.xlsx", "office read /w/order_batch_A.xlsx")
	st.noteSeen("Customer: Riverside Dental, PO NB-24102")
	if n := strayScript(st, `gui type e71 "สูตรบาคาร่า"`); !strings.Contains(n, "was a slip") {
		t.Fatalf("nudge = %q", n)
	}
	if n := strayScript(st, `gui type e71 "Riverside Dental"`); n != "" {
		t.Fatalf("a value from the document was taken for a slip: %q", n)
	}
	st.noteSeen("Kunde: Müller, Адрес: Москва")
	if n := strayScript(st, `gui type e9 "Москва"`); n != "" {
		t.Fatalf("a script the task does use was taken for a slip: %q", n)
	}
}

// Finding BQ (2026-10-20): the content of a file for an app the request names may be made with the tools; the app has
// to open, check and save it - asked for when the work is reported, never by refusing office create/update.
func TestToolsMayMakeTheContentAndTheNamedAppMustHandleIt(t *testing.T) {
	t.Setenv("DECURIX_SHARED_FOLDER", "")
	st := &operatorState{Request: "Make a deck from the attached brief, built in PowerPoint, and save it as q3_review.pptx.",
		Worked: []string{"office create /home/headless/work/q3_review.pptx - <<'EOF'\n{}\nEOF"}}
	if n := sharedAppUntouchedNudge(st); !strings.Contains(n, "no window of powerpoint was opened") || !strings.Contains(n, "gui open FILE") {
		t.Errorf("made with tools, never opened in the app: %q", n)
	}
	st.NamedAppOpened = true
	if n := sharedAppUnsavedNudge(st); !strings.Contains(n, "nothing was saved from powerpoint") {
		t.Errorf("opened, never saved from the app: %q", n)
	}
	st.SavedInApp = true
	if sharedAppUntouchedNudge(st) != "" || sharedAppUnsavedNudge(st) != "" {
		t.Error("made with tools, then opened and saved in the app, was sent back")
	}
	plain := &operatorState{Request: "Make a PowerPoint deck from the attached brief and save it as q3_review.pptx.",
		Worked: []string{"office create /home/headless/work/q3_review.pptx - <<'EOF'\n{}\nEOF"}}
	if sharedAppUntouchedNudge(plain) != "" {
		t.Error("a request that names no app to work in was sent to one")
	}
}

func TestAWindowsPathInDoubleQuotesKeepsItsBackslashes(t *testing.T) {
	got := typedValues(`gui type e12 "C:\Users\Administrator\Documents\Decurix demo"`)
	if len(got) != 1 || got[0] != `C:\Users\Administrator\Documents\Decurix demo` {
		t.Fatalf("got %q", got)
	}
	if got := typedValues(`gui type e12 "say \"hi\" for \$5"`); len(got) != 1 || got[0] != `say "hi" for $5` {
		t.Fatalf("escapes inside double quotes: got %q", got)
	}
}

func TestAuthoredWordsAreNotGuessesButTheirFiguresAreTraced(t *testing.T) {
	st := &operatorState{Request: "In Word, write a reply letter to the customer in the attached complaint: apologise, and confirm a replacement within 5 working days."}
	st.noteSeen(st.Request + "\nOrder 55821 arrived damaged. Address: 14 Lake View Road, Pune 411001")
	if n := groundingNudge(st, `gui type --append "Dear Ms Menon, we are sorry that order 55821 arrived damaged."`); n != "" {
		t.Fatalf("authored sentence refused: %s", n)
	}
	if n := groundingNudge(st, `gui type --append "15 October 2026
Dear Ms Menon, a replacement will reach 14 Lake View Road within 5 working days."`); n != "" {
		t.Fatalf("a letter's own date was refused: %s", n)
	}
	if n := groundingNudge(st, `gui type --append "A refund of 1,499.00 will follow."`); n == "" {
		t.Fatal("a figure nobody gave was let through in authored text")
	}
	data := &operatorState{Request: "Enter the attached invoice in the portal."}
	data.noteSeen(data.Request)
	if n := groundingNudge(data, `gui type e3 "Acme Widgets"`); n == "" {
		t.Fatal("data entry with an untraced value was let through")
	}
}

func TestValuesTypedAcrossTabsAreEachTheirOwn(t *testing.T) {
	got := typedValues(`gui type e9 "North\t812\nSouth\t655"`)
	want := []string{"North", "812", "South", "655"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// A question about how the app works is the web's to answer first; one only the person can answer is not.
func TestAHowToQuestionIsLookedUpFirst(t *testing.T) {
	for _, q := range []string{
		"How do I set the Under field to Sundry Creditors in TallyPrime?",
		"I can't find the menu option to freeze the header row.",
		"Which menu holds the chart wizard?",
		// Found live (2026-10-15): asked of the person, not looked up.
		"I'm blocked in a Tally purchase-voucher popup (Receipt Details). Please clear that popup on this worker's screen, then reply.",
	} {
		if !howToQuestionRe.MatchString(q) {
			t.Errorf("%q is a question about the app", q)
		}
	}
	for _, q := range []string{
		"Which supplier should I use for this bill: Kaveri or Kaveri Electricals?",
		"Please give me the password for the ERP.",
		"Should I submit the invoice or leave it as a draft?",
	} {
		if howToQuestionRe.MatchString(q) && !humanOnlyRe.MatchString(q) {
			t.Errorf("%q is the person's to answer", q)
		}
	}
}

// What a lookup prints is a source of values only when the request asks for something from the web.
func TestLookupOutputIsNotASourceOfValues(t *testing.T) {
	if !isLookup(`lookup "TallyPrime how to create a ledger"`) || !isLookup(`cd ~ && lookup x`) || isLookup(`echo lookup`) {
		t.Fatal("isLookup misread a command")
	}
	if researchRe.MatchString("Enter the attached vendor bill in ERPNext") {
		t.Fatal("entering a bill is not research")
	}
	if !researchRe.MatchString("Look up the current CEO of each company") {
		t.Fatal("looking up a CEO is research")
	}
}

// TestAQuestionAboutAnUnopenedAppSaysWhatWasTried: never "it may not be one of this desktop's apps" (round 5 P4).
func TestAQuestionAboutAnUnopenedAppSaysWhatWasTried(t *testing.T) {
	st := &operatorState{}
	if got := appOpenAttempts(st, "Word"); got != "I did not open Word at all." {
		t.Fatalf("nothing tried: %q", got)
	}
	st.Opens = []string{"gui open P4_onboarding_Nov/Offer_letter_template.docx -> exit 0 · Started soffice --nologo " +
		"--norestore P4_onboarding_Nov/Offer_letter_template.docx, but no new window appeared within 90 seconds."}
	got := appOpenAttempts(st, "Word")
	if !strings.Contains(got, "I did not open Word itself - I opened gui open P4_onboarding_Nov/Offer_letter_template.docx") ||
		!strings.Contains(got, "Started soffice") {
		t.Fatalf("opened another app: %q", got)
	}
	st.Opens = append(st.Opens, `gui open "Word (Host)" -> exit 1 · gui: the host app did not answer`)
	got = appOpenAttempts(st, "Word")
	if !strings.Contains(got, `I tried to open Word and it did not open: gui open "Word (Host)" -> exit 1 · gui: the host app did not answer.`) {
		t.Fatalf("tried and failed: %q", got)
	}
	if strings.Contains(got, "may not be one of this desktop's apps") {
		t.Fatalf("a guess: %q", got)
	}
}

// Found live (2026-10-21): asked to "send Suresh a message on ClientChat asking him about the differences", every
// wording was refused as untraced and the person was asked for the exact text.
func TestAMessageTheRequestAsksToSendIsAuthored(t *testing.T) {
	st := &operatorState{Request: "Reconcile the register against the invoices. Then send Suresh a message on ClientChat asking him about the differences."}
	st.noteSeen(st.Request + "\nInvoice 789 taxable 25,423.72\nRegister 789 24423.72\nInvoice 793 taxable 12,711.86")
	if n := groundingNudge(st, `gui type e7 "Hi Suresh, invoice 789 shows 25,423.72 but the register has 24,423.72, and 793 (12,711.86) is not in the register. Please check."`); n != "" {
		t.Fatalf("the asked-for message was refused: %s", n)
	}
	if n := groundingNudge(st, `gui type e7 "Hi Suresh, invoice 790 shows 9,999.00 in the register."`); n == "" {
		t.Fatal("a figure nobody gave was let through in the message")
	}
}

// Found live (2026-10-21): "Lift$2026" typed in double quotes reached the form as "Lift026".
func TestADollarBeforeADigitIsSentToStdin(t *testing.T) {
	if n := quotesNudge(`gui type e6 "Lift$2026"`); !strings.Contains(n, "verbatim on stdin") {
		t.Fatalf("a $ before a digit was let through: %q", n)
	}
	for _, ok := range []string{`gui type e6 'Lift$2026'`, `gui type e6 "Price 5 $"`, `gui type e6 "US$ 5"`} {
		if n := quotesNudge(ok); n != "" {
			t.Fatalf("%s was sent back: %q", ok, n)
		}
	}
}

// Found live (2026-10-25): the first and last day of the month a bill is dated in - a day book's from and to - were
// refused as guesses and the person was asked for "the exact dates". They are the period's edges; other days are not.
func TestAMonthsFirstAndLastDayAreTracedByADateInIt(t *testing.T) {
	set := datesIn("Bill VAI-3306 Date: 02-11-2026 Total 98,700.00")
	for v, want := range map[string]bool{"1-11-2026": true, "30-11-2026": true, "15-11-2026": false, "31-10-2026": false, "1-12-2026": false} {
		d, ok := canonicalDate(v)
		if !ok {
			t.Fatalf("%s is no date", v)
		}
		if got := set.holds(d) || set.boundsAMonth(d); got != want {
			t.Errorf("%s traced = %v, want %v", v, got, want)
		}
	}
}
