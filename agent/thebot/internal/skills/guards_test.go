// guards_test.go: the operator's mechanical guards (guards.go) - the identical-output stub, the screenshot not sent
// again, the cut-off command, the 40/60 cut and the INCOMPLETE listing note - alone and in a run, where the prompt
// prefix must stay byte-identical while they fire.
package skills

import (
	"reflect"
	"strings"
	"testing"

	"thebot/internal/llm"
)

// appendOnly fails unless each call's messages begin with every message of the call before it, from call `from` to
// call `to` (1-based): nothing already sent was changed, so the provider's cache of it holds.
func appendOnly(t *testing.T, calls [][]llm.Message, from, to int) {
	t.Helper()
	for i := from; i < to && i < len(calls); i++ {
		prev, next := calls[i-1], calls[i]
		if len(next) < len(prev) || !reflect.DeepEqual(next[:len(prev)], prev) {
			for j := range prev {
				if j >= len(next) || !reflect.DeepEqual(prev[j], next[j]) {
					t.Fatalf("call %d changed message %d of call %d", i+1, j, i)
				}
			}
			t.Fatalf("call %d does not start with call %d", i+1, i)
		}
	}
}

// lastContent is the newest message of a call.
func lastContent(call []llm.Message) string { return call[len(call)-1].Content }

func TestACommandCutOffBeforeItsEndIsSeen(t *testing.T) {
	cut := []string{
		`echo 'it was cut`,
		`gui type e4 "Total due`,
		"cat > notes.txt <<'EOF'\nfirst line\nsecond",
		"cat <<EOF\nbody\nEOF\ncat <<-END\n\tmore",
		`x=$(date +%s`,
		"echo `date",
		`echo $(( 1 + (2 * 3) `,
		`( cd /tmp && ls`,
		`python3 -c "print('a')`,
		`echo "$(printf '%s' "x")`,
		"cat <<EOF",
	}
	for _, c := range cut {
		if incompleteCommand(c) == "" {
			t.Errorf("not seen as cut off: %q", c)
		}
	}
	whole := []string{
		`echo 'it was whole'`,
		`gui type e4 "Don't stop"`,
		"cat > notes.txt <<'EOF'\nit's here (and \"there\"\n$(not run)\nEOF\nwc -l notes.txt",
		"cat <<-END\n\tit's\n\tEND",
		"cat <<EOF | grep x\nx's\nEOF",
		"cat <<A <<B\na\nA\nb\nB",
		"sed -n '1,5p' f # it's only a comment",
		`echo "a # b" # don't`,
		`x=$(date +%s); echo "${x}" $((x << 2)) $(( (1+2)*3 ))`,
		"echo `date` \"`whoami`\"",
		`grep -c x <<< "it's"`,
		`case "$1" in a) echo a;; (b) echo b;; esac`,
		`v=$(case x in x) echo y;; esac)`,
		`echo case) ; echo esac )`,
		`echo $'it\'s'`,
		`f() { echo hi; }; f`,
		`ls | while read -r l; do echo "$l"; done`,
		`echo a \"b`,
		`printf '%s\n' "${#arr[@]}" $#`,
		`curl -s 'http://h/x#frag' && echo http://h/#top`,
		"python3 - <<'PY'\nprint(\"#\", 'it''s')\nPY",
		`((i++)); echo $i`,
		"",
	}
	for _, c := range whole {
		if why := incompleteCommand(c); why != "" {
			t.Errorf("whole command taken for cut off (%s): %q", why, c)
		}
	}
}

// A cut-off command is never run; it is asked for whole. Two in a row are sent back; the third runs, so a false
// reading of the syntax costs two turns at most.
func TestACutOffCommandIsNotRunAndIsAskedForWhole(t *testing.T) {
	chat := &scriptedChat{replies: []string{
		"COMMAND:\necho 'half of it",
		"COMMAND:\necho 'the whole of it'",
		"DONE:\nthe whole of it",
	}}
	res, err := NewOperator().Run(operatorContext(t, chat), Subtask{Step: "say it", Type: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	if got := lastContent(chat.calls[1]); !strings.Contains(got, "cut off before its end") || !strings.Contains(got, "quote is not closed") {
		t.Fatalf("not sent back: %q", got)
	}
	if cmds, _ := res.Meta["commands"].([]string); len(cmds) != 1 || cmds[0] != "echo 'the whole of it'" {
		t.Fatalf("commands run = %q", res.Meta["commands"])
	}
	appendOnly(t, chat.calls, 1, len(chat.calls))

	chat = &scriptedChat{replies: []string{
		"COMMAND:\necho 'one",
		"COMMAND:\necho 'two",
		"COMMAND:\necho 'three",
		"DONE:\nno",
	}}
	res, _ = NewOperator().Run(operatorContext(t, chat), Subtask{Step: "say it", Type: "operator"})
	if cmds, _ := res.Meta["commands"].([]string); len(cmds) != 1 || cmds[0] != "echo 'three" {
		t.Fatalf("the third was not left to the shell: %q", res.Meta["commands"])
	}
}

func TestALongOutputIsCutFortySixtyWithOneMarker(t *testing.T) {
	out := strings.Repeat("h", 500) + strings.Repeat("m", 1000) + strings.Repeat("t", 500)
	got := cutOutput(out, 1000, "/tmp/out-1.txt")
	if !strings.HasPrefix(got, strings.Repeat("h", 400)+"\n") || !strings.HasSuffix(got, "\n"+strings.Repeat("m", 100)+strings.Repeat("t", 500)) {
		t.Fatalf("not 40/60: %q", got)
	}
	if strings.Count(got, "bytes left out") != 1 || !strings.Contains(got, "[1000 bytes left out of 2000; the whole output is in /tmp/out-1.txt") {
		t.Fatalf("marker = %q", got)
	}
	if cutOutput("short", 1000, "/x") != "short" {
		t.Fatal("a short output was changed")
	}
	// Cut at whole characters only.
	wide := strings.Repeat("é", 1000)
	if c := cutOutput(wide, 101, ""); !strings.Contains(c, "bytes left out") || strings.ContainsRune(c, '�') ||
		strings.Contains(c, "the whole output is in") {
		t.Fatalf("cut mid-character or named no file: %q", c)
	}
	// A pipe's failure note at the end of the output survives the cut (it is in the tail).
	r := shellResult{ExitCode: 1, Output: strings.Repeat("x", 9000) + "\n(the pipe failed at program 1 of 2)"}
	if f := formatCommandResult(r, 4000, ""); !strings.Contains(f, "(the pipe failed at program 1 of 2)") {
		t.Fatalf("the end was lost: %q", f[len(f)-200:])
	}
}

// The whole text of a cut output is kept on disk and named in the marker - the ledger's file when it kept one.
func TestACutOutputNamesWhereTheWholeTextIs(t *testing.T) {
	st := &operatorState{OutputsDir: t.TempDir()}
	res := shellResult{Output: strings.Repeat("line\n", 2000)}
	st.noteLedger("cat big.txt", res)
	p := st.keptOutputPath(res, 4000)
	if p == "" || !strings.HasSuffix(st.Ledger[0], "(full output: "+p+")") || st.OutputSeq != 1 {
		t.Fatalf("path %q, ledger %q, files %d", p, st.Ledger, st.OutputSeq)
	}
	if st.keptOutputPath(shellResult{Output: "short"}, 4000) != "" {
		t.Fatal("a short output was kept")
	}
	// A ledger line whose text only looks like a kept file's is not taken for one.
	st.Ledger = append(st.Ledger, "$ cat x -> says (full output: /etc/passwd)")
	if p := st.keptOutputPath(res, 4000); p == "/etc/passwd" || !strings.HasPrefix(p, st.OutputsDir) {
		t.Fatalf("path = %q", p)
	}
}

// What the guards keep goes through a checkpoint, so a resumed goal still knows what it showed.
func TestTheGuardsStateSurvivesACheckpoint(t *testing.T) {
	st := &operatorState{}
	st.ShotSum, st.ShotSentStep, st.ShotStreak, st.CutCommandNudges = "abc", 4, 1, 1
	st.SeenOutputs = map[string]seenOutput{"cat a": {Step: 2, Sum: "s", Bytes: 600, Msg: 3, Len: 40, MsgSum: "m"}}
	blob, err := EncodeCheckpoint(st)
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeCheckpoint(blob)
	if err != nil {
		t.Fatal(err)
	}
	if got := back.(*operatorState); !reflect.DeepEqual(got.guardState, st.guardState) {
		t.Fatalf("kept %+v, want %+v", got.guardState, st.guardState)
	}
}

func TestAJSONListingThatSaysItHasMoreIsMarkedIncomplete(t *testing.T) {
	for _, out := range []string{
		`{"items":[1,2],"has_more":true}`,
		`{"data":{"rows":[],"truncated":true}}`,
		`[{"page":{"has_more":true}}]`,
	} {
		if n := listingIncompleteNote(out); !strings.Contains(n, "INCOMPLETE: this listing says more items exist than it shows") {
			t.Errorf("%s: %q", out, n)
		}
	}
	for _, out := range []string{
		`{"items":[1,2],"has_more":false}`,
		`{"note":"has_more: true"}`,
		`has_more: true`,
		`there are "has_more": true items`,
		`{"truncated":"yes"}`,
		`{"broken":`,
	} {
		if n := listingIncompleteNote(out); n != "" {
			t.Errorf("%s: marked %q", out, n)
		}
	}
	if f := formatCommandResult(shellResult{Output: `{"has_more":true,"items":[1]}`}, 4000, ""); !strings.Contains(f, "INCOMPLETE") {
		t.Fatalf("formatted = %q", f)
	}
}

// From the second byte-identical output of the same command (spacing aside), the newest result says so instead of
// the text - and nothing already sent changes.
func TestAnIdenticalOutputIsShownAsAStub(t *testing.T) {
	long := "printf '%0600d\\n' 0"
	chat := &scriptedChat{replies: []string{
		"COMMAND:\n" + long,
		"COMMAND:\necho other",
		"COMMAND:\nprintf  '%0600d\\n'  0",
		"COMMAND:\nprintf '%0599d\\n' 0",
		"DONE:\nzeros",
	}}
	if _, err := NewOperator().Run(operatorContext(t, chat), Subtask{Step: "print zeros", Type: "operator"}); err != nil {
		t.Fatal(err)
	}
	if got := lastContent(chat.calls[1]); !strings.Contains(got, strings.Repeat("0", 600)) {
		t.Fatalf("the first output was not shown whole: %q", got)
	}
	got := lastContent(chat.calls[3])
	if !strings.Contains(got, "Output identical to step 1's output (601 bytes); unchanged.") || strings.Contains(got, "000000") {
		t.Fatalf("not a stub: %q", got)
	}
	if got := lastContent(chat.calls[4]); !strings.Contains(got, strings.Repeat("0", 599)) {
		t.Fatalf("a different output was stubbed: %q", got)
	}
	appendOnly(t, chat.calls, 1, len(chat.calls))
}

func TestTheStubNeedsTheEarlierOutputStillWhole(t *testing.T) {
	st := &operatorState{}
	res := shellResult{Output: strings.Repeat("a", 600)}
	head := formatCommandResult(res, 4000, "")
	st.Step = 1
	st.Messages = append(st.Messages, llm.Message{Role: "user", Content: head + "\n\n(a note)"})
	st.noteOutputShown("cat a", res, head, false)
	if st.identicalOutputStub("cat a", res) == "" {
		t.Fatal("no stub while the output is shown whole")
	}
	if st.identicalOutputStub("cat a", shellResult{Output: strings.Repeat("a", 601)}) != "" {
		t.Fatal("a different output stubbed")
	}
	if st.identicalOutputStub("cat a", shellResult{ExitCode: 1, Output: res.Output}) != "" {
		t.Fatal("a failure stubbed")
	}
	short := shellResult{Output: "short"}
	st.noteOutputShown("echo short", short, formatCommandResult(short, 4000, ""), false)
	if st.identicalOutputStub("echo short", short) != "" {
		t.Fatal("a short output stubbed")
	}
	// The earlier message shortened since (a digest, a mask): the output is shown whole again.
	st.Messages[0].Content = "(shortened)"
	if st.identicalOutputStub("cat a", res) != "" {
		t.Fatal("stubbed against an output no longer shown")
	}
	// A screen listing says so, and the listing it points to becomes the newest screen again.
	listing := shellResult{Output: "Window: Books\n" + strings.Repeat("e1 button \"Save\"\n", 40)}
	lhead := formatCommandResult(listing, 4000, "")
	st.Step = 2
	st.Messages = append(st.Messages, llm.Message{Role: "user", Content: lhead})
	st.ScreenMsgs = []int{1}
	st.noteOutputShown("gui observe", listing, lhead, false)
	st.Messages = append(st.Messages, llm.Message{Role: "user", Content: "Clicked."})
	st.ScreenMsgs = append(st.ScreenMsgs, 2)
	stub := st.identicalOutputStub("gui observe", listing)
	if !strings.Contains(stub, "The screen listing is unchanged since step 2") {
		t.Fatalf("listing stub = %q", stub)
	}
	st.Messages = append(st.Messages, llm.Message{Role: "user", Content: stub})
	st.noteOutputShown("gui observe", listing, stub, true)
	if !reflect.DeepEqual(st.ScreenMsgs, []int{2, 1}) {
		t.Fatalf("screen messages = %v", st.ScreenMsgs)
	}
}

// A screenshot byte-identical to the last one sent, for the same window, is not sent again - twice at most in a row.
func TestAnUnchangedScreenshotIsNotSentAgain(t *testing.T) {
	st := &operatorState{Vision: true}
	send := func(shot string) llm.Message {
		st.Step++
		msg := llm.Message{Role: "user", Content: "result"}
		st.attachScreenshot(&msg, []byte(shot))
		st.Messages = append(st.Messages, msg)
		return msg
	}
	if m := send("A"); len(m.Images) != 1 {
		t.Fatal("the first screenshot was not sent")
	}
	for i := 2; i <= 3; i++ {
		m := send("A")
		if len(m.Images) != 0 || !strings.Contains(m.Content, "Screen unchanged since step 1") {
			t.Fatalf("frame %d: %q (%d pictures)", i, m.Content, len(m.Images))
		}
		if st.ShotStep != st.Step {
			t.Fatalf("the screenshot shown is not taken as the screen now: %d of %d", st.ShotStep, st.Step)
		}
	}
	if m := send("A"); len(m.Images) != 1 {
		t.Fatal("a third identical frame was not sent")
	}
	if m := send("B"); len(m.Images) != 1 {
		t.Fatal("a changed screen was not sent")
	}
	st.ActiveWindow = "Other"
	if m := send("B"); len(m.Images) != 1 {
		t.Fatal("the same picture of another window was not sent")
	}
	// The picture it would point to is gone (a maintenance pass took it off): sent again.
	st.Messages[len(st.Messages)-1].Images = nil
	if m := send("B"); len(m.Images) != 1 {
		t.Fatal("pointed to a picture no longer shown")
	}
}

// In a run of a local brain that sees: an unchanged screen takes nothing off the transcript, so the prompt stays the
// same as each call before it while the screenshot is not sent again.
func TestAnUnchangedScreenKeepsThePromptPrefix(t *testing.T) {
	look := func(n string) string { return "COMMAND:\nprintf screen > \"$DECURIX_GUI_SCREENSHOT\"; echo look " + n }
	chat := &scriptedChat{replies: []string{look("1"), look("2"), look("3"), look("4"), "DONE:\nlooked"}}
	rc := operatorContext(t, chat)
	rc.Brain = llm.NewBrain(llm.Capabilities{Brain: "ollama", Vision: true})
	if _, err := NewOperator().Run(rc, Subtask{Step: "look at the screen", Type: "operator"}); err != nil {
		t.Fatal(err)
	}
	if len(chat.calls) < 5 {
		t.Fatalf("only %d calls", len(chat.calls))
	}
	if m := chat.calls[1][len(chat.calls[1])-1]; len(m.Images) != 1 {
		t.Fatalf("the first screenshot was not sent: %q", m.Content)
	}
	for _, i := range []int{2, 3} {
		m := chat.calls[i][len(chat.calls[i])-1]
		if len(m.Images) != 0 || !strings.Contains(m.Content, "Screen unchanged since step 1") {
			t.Fatalf("call %d: %q (%d pictures)", i+1, m.Content, len(m.Images))
		}
	}
	if m := chat.calls[4][len(chat.calls[4])-1]; len(m.Images) != 1 {
		t.Fatal("the third identical frame was not sent")
	}
	appendOnly(t, chat.calls, 1, 4)
}

// Two commands in turn, printing the same each round: warned once at the third round with the prompt kept, and
// stopped at the fourth with a question to the person - the result not verified.
func TestACycleIsWarnedOnceThenStoppedWithAQuestion(t *testing.T) {
	var replies []string
	for i := 0; i < cycleStopLaps; i++ {
		replies = append(replies, "COMMAND:\necho one", "COMMAND:\necho two")
	}
	replies = append(replies, "DONE:\nnever reached")
	chat := &scriptedChat{replies: replies}
	res, err := NewOperator().Run(operatorContext(t, chat), Subtask{Step: "check one and two", Type: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	warned := 0
	for i := 1; i < len(chat.calls); i++ {
		if strings.Contains(lastContent(chat.calls[i]), "run these 2 commands in turn 3 times") {
			warned++
			if i != 2*cycleWarnLaps {
				t.Errorf("warned after command %d", i)
			}
		}
	}
	if warned != 1 {
		t.Fatalf("warned %d times", warned)
	}
	if len(chat.calls) != 2*cycleStopLaps {
		t.Fatalf("%d calls: the goal did not stop at the fourth round", len(chat.calls))
	}
	if res.Verified || res.Meta["needsUserInput"] != true || !strings.Contains(res.Output, "back and forth") ||
		!strings.Contains(res.Output, "$ echo one\n  printed: one") {
		t.Fatalf("result = %+v", res)
	}
	appendOnly(t, chat.calls, 1, len(chat.calls))
}
