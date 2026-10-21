package skills

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"thebot/internal/llm"
)

// A portal stage's shape: look at the form, type into its fields, save, look again. Old listings are shortened once a
// new listing arrives; the newest stays whole; office read output stays whole; every re-send is the previous one
// plus what is new, with at most the superseded listing changed; the guard still traces values from shortened ones.
func portalStage(t *testing.T) *scriptedChat {
	t.Helper()
	stubWorkerTools(t,
		`case "$1" in
observe) echo "Screen 1280x720. Active window: \"Order $$ - Chromium\""; i=1; while [ $i -le 120 ]; do echo "e$i textbox \"Field $i\" value \"Customer PO WA-1810 line $i\""; i=$((i+1)); done;;
click) echo "clicked button \"Save\" ($2) - window \"Order saved\" opened";;
*) echo "typed into $2";;
esac
`,
		`echo "price_list_Oct2026.xlsx: sheet Prices"; echo "MUG-12 | Ceramic mug 12oz | 4.20"`, "")
	chat := &scriptedChat{replies: []string{
		"COMMAND:\noffice read /w/price_list_Oct2026.xlsx",
		"COMMAND:\ngui observe",
		"COMMAND:\ngui type e4 \"WA-1810\"",
		"COMMAND:\ngui type e5 \"4.20\"",
		"COMMAND:\ngui click e9",
		"COMMAND:\ngui observe",
		"COMMAND:\ngui type e7 \"WA-1810\"",
		"COMMAND:\ngui observe",
		"DONE:\nEntered.",
	}}
	return chat
}

func TestOldScreensAreShortenedOnceAndEverythingElseStaysWhole(t *testing.T) {
	chat := portalStage(t)
	rc := operatorContext(t, chat)
	if _, err := NewOperator().Run(rc, Subtask{Step: "Enter the order WA-1810 for MUG-12 at 4.20 from /w/price_list_Oct2026.xlsx.", Type: "operator"}); err != nil {
		t.Fatal(err)
	}
	last := chat.calls[len(chat.calls)-1]
	whole, short, office := 0, 0, 0
	for _, m := range last {
		switch {
		case strings.Contains(m.Content, staleNote):
			short++
			if !strings.Contains(m.Content, "(full output: ") {
				t.Errorf("a shortened output does not say where its full text is: %q", m.Content)
			}
		case strings.Contains(m.Content, "e1 textbox"):
			whole++
		case strings.Contains(m.Content, "MUG-12 | Ceramic mug 12oz | 4.20"):
			office++
		}
	}
	if whole != 1 || office != 1 || short < 2 {
		t.Errorf("whole listings %d (want 1, the newest), office reads %d (want 1, whole), shortened %d", whole, office, short)
	}
	// The full text of a shortened listing is on disk for the model to read again.
	for _, m := range last {
		if i := strings.Index(m.Content, "(full output: "); i >= 0 {
			path := strings.TrimSuffix(strings.TrimPrefix(m.Content[i:], "(full output: "), ")")
			data, err := os.ReadFile(path)
			if err != nil || !strings.Contains(string(data), "e1 textbox") {
				t.Errorf("full output %s: %v", path, err)
			}
			break
		}
	}
	// Each re-send is the one before plus what is new, with at most one earlier message changed (the listing just
	// superseded) - so the provider's cache keeps everything before it.
	for i := 1; i < len(chat.calls); i++ {
		before, now := chat.calls[i-1], chat.calls[i]
		changed := 0
		for j := range before {
			if before[j].Content != now[j].Content {
				changed++
			}
		}
		if changed > 1 {
			t.Errorf("turn %d changed %d earlier messages", i+1, changed)
		}
	}
}

func TestTheGuardTracesValuesFromAShortenedListing(t *testing.T) {
	st := groundingState("Enter the order for the customer.")
	listing := "Screen 1280x720.\ne4 textbox \"Customer PO\" value \"WA-1810\"\ne5 combobox \"Product\" option \"MUG-12 - Ceramic mug 12oz\""
	st.noteSeen(listing) // what execute notes for every command
	st.Messages = append(st.Messages, llm.Message{Role: "user", Content: "Command execution result:\n\nexit_code: 0\n\noutput:\n" + listing})
	st.ScreenMsgs = []int{len(st.Messages) - 1}
	st.digestStaleScreens()
	if strings.Contains(st.Messages[len(st.Messages)-1].Content, "MUG-12 - Ceramic") {
		t.Fatal("the listing was not shortened")
	}
	for _, cmd := range []string{`gui type e9 "WA-1810"`, `gui select e5 "MUG-12 - Ceramic mug 12oz"`} {
		if n := groundingNudge(st, cmd); n != "" {
			t.Errorf("%s was refused after its listing was shortened: %q", cmd, n)
		}
	}
}

// TestMeasureAGUIStage prints what a GUI stage re-sends with and without shortened listings (MEASURE=1): 30 turns,
// a 6,000-character listing every third turn, 300-character action outputs between, an 8,000-token fixed prefix.
func TestMeasureAGUIStage(t *testing.T) {
	if os.Getenv("MEASURE") == "" {
		t.Skip("set MEASURE=1")
	}
	const prefix, listing, action, turns = 32000, 6000, 300, 30
	run := func(digest bool) (total int) {
		var outputs []int
		var screens []int
		for turn := 0; turn < turns; turn++ {
			size := action
			if turn%3 == 0 {
				size = listing
				if digest {
					for _, i := range screens {
						outputs[i] = 400
					}
					screens = nil
				}
			}
			outputs = append(outputs, size+60) // + the reply that asked for it
			if size == listing {
				screens = append(screens, len(outputs)-1) // only listings are shortened
			}
			sum := prefix
			for _, o := range outputs {
				sum += o
			}
			total += sum
		}
		return total / 4
	}
	before, after := run(false), run(true)
	fmt.Printf("GUI stage, %d turns: %d tokens re-sent today, %d with shortened listings (%.0f%% less); last turn %d -> %d\n",
		turns, before, after, 100*float64(before-after)/float64(before), (32000+10*6000+20*300+30*60)/4, (32000+6000+29*460)/4)
}
