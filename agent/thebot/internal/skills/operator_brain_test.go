package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"thebot/internal/llm"
)

// A brain that reads images is shown the screen after an on-screen command:
// the operator tells the gui command where to leave it (a stand-in command
// here writes that file the way gui does), attaches it to the command's
// result, and keeps only the newest one.
func TestAVisionBrainIsShownTheScreenAfterEachScreenCommand(t *testing.T) {
	chat := &scriptedChat{replies: []string{
		"COMMAND:\n" + forShell(
			`printf first > "$DECURIX_GUI_SCREENSHOT"; echo "clicked; scale $DECURIX_GUI_SCREENSHOT_SCALE; text $DECURIX_GUI_TEXT_CHARS"`,
			`[IO.File]::WriteAllText($env:DECURIX_GUI_SCREENSHOT, 'first'); echo "clicked; scale $env:DECURIX_GUI_SCREENSHOT_SCALE; text $env:DECURIX_GUI_TEXT_CHARS"`),
		"COMMAND:\n" + forShell(
			`printf second > "$DECURIX_GUI_SCREENSHOT"; echo typed`,
			`[IO.File]::WriteAllText($env:DECURIX_GUI_SCREENSHOT, 'second'); echo typed`),
		`COMMAND:
echo not-a-screen-command`,
		"DONE:\nSaved.",
	}}
	rc := operatorContext(t, chat)
	rc.Brain = llm.NewBrain(llm.Capabilities{Brain: "claude", Vision: true, Hosted: true})

	res, err := NewOperator().Run(rc, Subtask{Step: "save the form", Type: "operator"})
	if err != nil || res.Output != "Saved." {
		t.Fatalf("result = %+v, %v", res, err)
	}

	if contract := chat.calls[0][len(chat.calls[0])-1].Content; !strings.Contains(contract, "shown a screenshot") {
		t.Errorf("the contract does not tell a vision brain about screenshots:\n%s", contract)
	}

	first := chat.calls[1][len(chat.calls[1])-1]
	if len(first.Images) != 1 || string(first.Images[0]) != "first" {
		t.Fatalf("first result images = %q", first.Images)
	}
	// A hosted brain gets the whole screen and a larger element list.
	if !strings.Contains(first.Content, "scale 1; text 12000") {
		t.Errorf("gui was not given the hosted budgets: %s", first.Content)
	}

	turn3 := chat.calls[2]
	if got := turn3[len(turn3)-1].Images; len(got) != 1 || string(got[0]) != "second" {
		t.Fatalf("second result images = %q", got)
	}
	shown := 0
	for _, m := range turn3 {
		shown += len(m.Images)
	}
	if shown != 1 {
		t.Errorf("%d screenshots in the transcript, want only the newest", shown)
	}

	// A command that is not a screen command leaves no file, so it gets no
	// stale picture.
	if got := chat.calls[3][len(chat.calls[3])-1].Images; len(got) != 0 {
		t.Errorf("a stale screenshot was attached to a non-screen command")
	}
}

// A text-only local brain gets none of that: no screenshot file is asked
// for, its budgets stay small, and scanned documents are read to it by OCR.
func TestATextBrainIsGivenTextOnly(t *testing.T) {
	chat := &scriptedChat{replies: []string{
		"COMMAND:\n" + forShell(
			`echo "shot=[$DECURIX_GUI_SCREENSHOT] office=$DECURIX_OFFICE_IMAGES text=$DECURIX_GUI_TEXT_CHARS"`,
			`echo "shot=[$env:DECURIX_GUI_SCREENSHOT] office=$env:DECURIX_OFFICE_IMAGES text=$env:DECURIX_GUI_TEXT_CHARS"`),
		"DONE:\nok",
	}}
	rc := operatorContext(t, chat)
	rc.Brain = llm.NewBrain(llm.Capabilities{Brain: "ollama"})

	if _, err := NewOperator().Run(rc, Subtask{Step: "look", Type: "operator"}); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(chat.calls[0][len(chat.calls[0])-1].Content, "screenshot") {
		t.Error("a text brain was told it would see screenshots")
	}
	out := chat.calls[1][len(chat.calls[1])-1]
	if !strings.Contains(out.Content, "shot=[] office=ocr text=3500") || len(out.Images) != 0 {
		t.Errorf("text brain env/result = %s (%d images)", out.Content, len(out.Images))
	}
}

// Document pages are kept apart from screenshots: a later screenshot must
// not push the scanned invoice out of view.
func TestDocumentPagesSurviveLaterScreenshots(t *testing.T) {
	dir := t.TempDir()
	page := filepath.Join(dir, "page-1.png")
	if err := os.WriteFile(page, []byte("invoice-page"), 0o644); err != nil {
		t.Fatal(err)
	}

	chat := &scriptedChat{replies: []string{
		"COMMAND:\necho 'invoice.pdf: 1 page (it is a scan - read it from the pictures).'; echo '[page image: " + page + "]'; echo \"mode " + forShell("$DECURIX_OFFICE_IMAGES", "$env:DECURIX_OFFICE_IMAGES") + "\"",
		"COMMAND:\n" + forShell(
			`printf screen > "$DECURIX_GUI_SCREENSHOT"; echo typed`,
			`[IO.File]::WriteAllText($env:DECURIX_GUI_SCREENSHOT, 'screen'); echo typed`),
		"DONE:\nKeyed in.",
	}}
	rc := operatorContext(t, chat)
	rc.Brain = llm.NewBrain(llm.Capabilities{Brain: "azure", Vision: true, Hosted: true})

	if _, err := NewOperator().Run(rc, Subtask{Step: "key in the invoice", Type: "operator"}); err != nil {
		t.Fatal(err)
	}

	read := chat.calls[1][len(chat.calls[1])-1]
	if len(read.Images) != 1 || string(read.Images[0]) != "invoice-page" || !strings.Contains(read.Content, "mode attach") {
		t.Fatalf("document result = %s (%q)", read.Content, read.Images)
	}

	var pages, screens int
	for _, m := range chat.calls[2] {
		for _, img := range m.Images {
			switch string(img) {
			case "invoice-page":
				pages++
			case "screen":
				screens++
			}
		}
	}
	if pages != 1 || screens != 1 {
		t.Errorf("after a screenshot: %d document pages, %d screenshots; want both kept", pages, screens)
	}
}

// A hosted brain gets source and application verification rules for any task;
// a local brain's contract stays as small as before.
func TestAHostedBrainIsGivenGenericTaskRules(t *testing.T) {
	for _, tc := range []struct {
		caps llm.Capabilities
		want bool
	}{
		{llm.Capabilities{Brain: "azure", Vision: true, Hosted: true}, true},
		{llm.Capabilities{Brain: "ollama"}, false},
	} {
		chat := &scriptedChat{replies: []string{"DONE:\nok"}}
		rc := operatorContext(t, chat)
		rc.Brain = llm.NewBrain(tc.caps)
		if _, err := NewOperator().Run(rc, Subtask{Step: "update the project request", Type: "operator"}); err != nil {
			t.Fatal(err)
		}
		contract := chat.calls[0][len(chat.calls[0])-1].Content
		if got := strings.Contains(contract, "Match records using identifiers and relevant attributes"); got != tc.want {
			t.Errorf("%s: rules in the contract = %v, want %v", tc.caps.Brain, got, tc.want)
		}
	}
}
