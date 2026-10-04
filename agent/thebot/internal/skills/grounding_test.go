package skills

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"thebot/internal/llm"
)

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
