package skills

import (
	"path/filepath"
	"strings"
	"testing"

	"thebot/internal/llm"
)

// Found live (2026-11-08, twice, on a tax-code form): at a held Save, the review's "Still open" lines said fields were
// empty that gui observe showed filled. The chain "gui type ... && gui click Save" held at Save recorded nothing, so
// the review was shown the look from before the typing. The review reads the window's latest look, or says it was not
// read - never an earlier one.

const formWindow = "New Tax Template - Chromium"

// typingFormGui is a stand-in gui: the form's Rate is empty until something is typed; a click on Save is held.
func typingFormGui(t *testing.T) {
	typed := filepath.ToSlash(filepath.Join(t.TempDir(), "typed"))
	stubWorkerTools(t, `case "$1" in
observe) v=""; [ -f '`+typed+`' ] && v="18"
  echo 'Screen 1280x720. Active window: "`+formWindow+`"'
  echo '== `+formWindow+` =='
  echo "e5 textbox \"Rate\" value=\"$v\"";;
type) touch '`+typed+`'; echo 'typed into e5';;
click) echo 'NEEDS_APPROVAL: Approval needed before I click button "Save" (e9) in "`+formWindow+`": it saves a record. Nothing was clicked.'; exit 3;;
*) echo;;
esac
`, "", "")
}

// reviewedFacts is what the reviewer was sent (the call made with its brief), or "".
func reviewedFacts(chat *scriptedChat) string {
	for _, call := range chat.calls {
		if len(call) == 2 && call[0].Content == reviewSystem {
			return call[1].Content
		}
	}
	return ""
}

func TestAtAHeldSaveTheReviewReadsTheFormAsTypedNotAnEarlierLook(t *testing.T) {
	typingFormGui(t)
	chat := &scriptedChat{replies: []string{`{"issues": []}`}}
	rc := operatorContext(t, chat)
	o := NewOperator()
	st := &operatorState{Hosted: true, Request: "Make the tax template: Rate 18%."}
	if held := o.execute(rc, st, "gui observe"); held != "" {
		t.Fatalf("held: %s", held)
	}
	reason := o.execute(rc, st, `gui type e5 "18" && gui click e9`)
	if reason == "" {
		t.Fatal("the stand-in's Save was not held")
	}
	if !st.typedSinceLook(formWindow) {
		t.Fatal("the typing ahead of the held press was not noted")
	}
	o.weighJudgements(rc, st, nil, "About to save the template.", reason)
	facts := reviewedFacts(chat)
	// (The commands' log keeps what the first look printed, in its place in time; the screen is the listing.)
	screens := facts
	if i := strings.Index(screens, "THE NEWEST LISTING"); i >= 0 {
		screens = screens[i:]
	}
	if i := strings.Index(screens, "COMMANDS AND WHAT EACH PRINTED"); i >= 0 {
		screens = screens[:i]
	}
	if !strings.Contains(screens, `"Rate" value="18"`) || strings.Contains(screens, `"Rate" value=""`) {
		t.Fatalf("the review was not shown the form as typed:\n%s", facts)
	}
}

func TestALookFromBeforeTheTypingIsShownAsNotRead(t *testing.T) {
	st := &operatorState{Request: "Make the tax template.", ActiveWindow: formWindow}
	st.noteListing("gui observe", "Screen 1280x720. Active window: \""+formWindow+"\"\n"+`e5 textbox "Rate" value=""`)
	st.Step++ // the held chain: typed, then stopped at Save
	st.noteHeldScreen(`gui type e5 "18" && gui click e9`, shellResult{ExitCode: 3, Output: "NEEDS_APPROVAL: ..."})
	facts := st.reviewFacts("About to save.", nil, "click Save")
	if strings.Contains(facts, `"Rate" value=""`) || !strings.Contains(facts, "NOT READ since the work typed into it") {
		t.Fatalf("an earlier look was shown as the form:\n%s", facts)
	}
}

func TestTheReviewIsShownOnlyAScreenshotOfTheScreenAsItIs(t *testing.T) {
	shot := func(b string) llm.Message {
		return llm.Message{Role: "user", Content: "x" + screenshotNote, Images: [][]byte{[]byte(pngHead + b)},
			ImageLabels: []string{"the screen as this command left it"}}
	}
	st := &operatorState{Vision: true, Messages: []llm.Message{{Role: "user", Content: "x"},
		{Role: "user", Content: "page", Images: [][]byte{[]byte(pngHead + "page")}, ImageLabels: []string{"bill.pdf p1"}},
		shot("before typing"), shot("after typing")}, ShotStep: 3, ScreenStep: 3}
	imgs, labels := st.lastPictures(3)
	if len(imgs) != 2 || string(imgs[0]) != pngHead+"page" || string(imgs[1]) != pngHead+"after typing" {
		t.Fatalf("pictures: %d %v", len(imgs), labels)
	}
	st.ScreenStep = 4 // a screen command since, with no screenshot (held at the gate)
	if imgs, _ := st.lastPictures(3); len(imgs) != 1 || string(imgs[0]) != pngHead+"page" {
		t.Fatalf("a screenshot from before the screen changed was shown: %d", len(imgs))
	}
	st.lookShot, st.lookShotStep = []byte(pngHead+"now"), 4
	if imgs, labels := st.lastPictures(3); len(imgs) != 2 || string(imgs[1]) != pngHead+"now" ||
		!strings.Contains(labels[1], "looked at before this review") {
		t.Fatalf("the look before the commit was not shown: %d %v", len(imgs), labels)
	}
}
