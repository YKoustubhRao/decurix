package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"thebot/internal/llm"
)

// scriptedChat answers each call with the next canned reply and records
// what it was sent.
type scriptedChat struct {
	replies []string
	calls   [][]llm.Message
}

func (s *scriptedChat) Name() string                         { return "scripted" }
func (s *scriptedChat) IsReachable(ctx context.Context) bool { return true }
func (s *scriptedChat) Chat(ctx context.Context, model string, messages []llm.Message, opts llm.Options) (llm.Result, error) {
	s.calls = append(s.calls, append([]llm.Message(nil), messages...))
	if len(s.calls) > len(s.replies) {
		return llm.Result{Text: "DONE: out of script"}, nil
	}
	return llm.Result{Text: s.replies[len(s.calls)-1]}, nil
}

func operatorContext(t *testing.T, provider llm.Provider) RunContext {
	t.Helper()
	return RunContext{Ctx: context.Background(), Provider: provider, Model: "m", WorkDir: t.TempDir(), OnLog: func(string) {}}
}

func TestOperatorRunsCommandsAndAnswersFromWhatTheyPrinted(t *testing.T) {
	chat := &scriptedChat{replies: []string{
		"COMMAND:\nls | wc -l",
		"DONE:\nThere are 3 files.",
	}}
	rc := operatorContext(t, chat)
	for _, name := range []string{"a", "b", "c"} {
		if err := os.WriteFile(filepath.Join(rc.WorkDir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	res, err := NewOperator().Run(rc, Subtask{Step: "how many files are here?", Type: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Verified || res.Output != "There are 3 files." {
		t.Fatalf("result = %+v", res)
	}
	// The command really ran, in the working directory, and its real
	// output reached the model on the next turn.
	last := chat.calls[1][len(chat.calls[1])-1].Content
	if !strings.Contains(last, "exit_code: 0") || !strings.Contains(last, "3") {
		t.Errorf("second turn was not shown the command's real output:\n%s", last)
	}
	// Append-only transcript: turn two starts with everything turn one sent.
	for i, m := range chat.calls[0] {
		if chat.calls[1][i] != m {
			t.Fatalf("turn two rewrote message %d - the prompt cache would be lost", i)
		}
	}
}

// A model that writes COMMAND: and then predicts a DONE: must have only
// the command run - the prediction is for output it has not seen.
func TestOperatorIgnoresAPredictedAnswerAfterACommand(t *testing.T) {
	chat := &scriptedChat{replies: []string{
		"COMMAND:\necho real-output\nDONE:\n5.10.124 (guessed)",
		"DONE:\nreal-output",
	}}
	res, _ := NewOperator().Run(operatorContext(t, chat), Subtask{Step: "print it", Type: "operator"})
	if res.Output != "real-output" {
		t.Errorf("Output = %q, want the answer given after seeing real output", res.Output)
	}
	if cmds, _ := res.Meta["commands"].([]string); len(cmds) != 1 || cmds[0] != "echo real-output" {
		t.Errorf("commands = %v, want exactly the command without the predicted DONE folded in", res.Meta["commands"])
	}
	// The guess must never be shown back to the model, or it reports it.
	for _, m := range chat.calls[1] {
		if strings.Contains(m.Content, "guessed") {
			t.Errorf("the predicted answer stayed in the transcript: %q", m.Content)
		}
	}
}

func TestOperatorPushesBackOnceOnQueryBeforeTrying(t *testing.T) {
	chat := &scriptedChat{replies: []string{
		"HELP:\nWhy do you want to know the desktop name?",
		"DONE:\nThis desktop is called kilo.",
	}}
	res, _ := NewOperator().Run(operatorContext(t, chat), Subtask{Step: "whats your desktop name?", Type: "operator"})
	if !res.Verified || res.Output != "This desktop is called kilo." {
		t.Errorf("result = %+v, want the answer after one push back", res)
	}
}

// Once pushed back, a second HELP is real - the agent must always be able
// to escalate, and HELP is also how it asks for approval.
func TestOperatorDeliversASecondQueryAndCanResumeWithTheReply(t *testing.T) {
	chat := &scriptedChat{replies: []string{
		"HELP:\nWhich project should I delete?",
		"HELP:\nThere are two projects, alpha and beta. Which one?",
		"COMMAND:\necho deleting beta",
		"DONE:\nDeleted beta.",
	}}
	op := NewOperator()
	rc := operatorContext(t, chat)

	res, _ := op.Run(rc, Subtask{Step: "delete the project", Type: "operator"})
	if res.Meta["needsUserInput"] != true || res.Checkpoint == nil {
		t.Fatalf("result = %+v, want a question with a checkpoint", res)
	}

	rc.Resume = res.Checkpoint
	rc.HumanReply = "beta"
	res, _ = op.Run(rc, Subtask{Step: "delete the project", Type: "operator"})
	if !res.Verified || res.Output != "Deleted beta." {
		t.Fatalf("resumed result = %+v", res)
	}
	sawReply := false
	for _, m := range chat.calls[2] {
		if strings.Contains(m.Content, "The human replied: beta") {
			sawReply = true
		}
	}
	if !sawReply {
		t.Error("the human's reply never reached the resumed transcript")
	}
}

func TestOperatorEndsARepeatLoopWithTheRealOutput(t *testing.T) {
	chat := &scriptedChat{replies: []string{
		"COMMAND:\necho 6.6.87.2",
		"COMMAND:\necho 6.6.87.2",
		"COMMAND:\necho 6.6.87.2",
	}}
	res, _ := NewOperator().Run(operatorContext(t, chat), Subtask{Step: "kernel version?", Type: "operator"})
	if !res.Verified || strings.TrimSpace(res.Output) != "6.6.87.2" {
		t.Errorf("result = %+v, want the command's own output as the answer", res)
	}
	if len(chat.calls) != 3 {
		t.Errorf("made %d model calls, want 3 (run, nudge, stop)", len(chat.calls))
	}
}

func TestOperatorTakesAnUnmarkedReplyAsTheAnswerEventually(t *testing.T) {
	chat := &scriptedChat{replies: []string{"241", "241", "241", "241"}}
	res, _ := NewOperator().Run(operatorContext(t, chat), Subtask{Step: "how many files in /etc?", Type: "operator"})
	if !res.Verified || res.Output != "241" {
		t.Errorf("result = %+v, want the unmarked reply salvaged", res)
	}
	if !strings.Contains(chat.calls[1][len(chat.calls[1])-1].Content, `started with "241"`) {
		t.Error("the format nudge did not quote the model's own reply back to it")
	}
}

func TestOperatorPausesForApprovalAndResumes(t *testing.T) {
	chat := &scriptedChat{replies: []string{
		"COMMAND:\ngit push origin main",
		"DONE:\nPushed.",
	}}
	op := NewOperator()
	rc := operatorContext(t, chat)
	// A fake git so the approved command has something harmless to run.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\necho pushed-for-real\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	res, _ := op.Run(rc, Subtask{Step: "push my branch", Type: "operator"})
	if res.Meta["needsApproval"] != true || res.Meta["action"] != "git push origin main" || res.Checkpoint == nil {
		t.Fatalf("result = %+v, want a paused approval for the push", res)
	}
	if cmds, _ := res.Meta["commands"].([]string); len(cmds) != 0 {
		t.Fatalf("commands = %v - the gated command ran before approval", cmds)
	}

	rc.Resume = res.Checkpoint
	rc.HumanReply = "yes"
	rc.Approved = func(action string) bool { return action == "git push origin main" }
	res, _ = op.Run(rc, Subtask{Step: "push my branch", Type: "operator"})
	if !res.Verified || res.Output != "Pushed." {
		t.Fatalf("resumed result = %+v", res)
	}
	if !strings.Contains(chat.calls[1][len(chat.calls[1])-1].Content, "pushed-for-real") {
		t.Error("the approved command did not actually run on resume")
	}
}

// "gui click e35" gives internal/safety nothing to match; only Decurix's
// screen gate knows e35 is a Send button. Its exit code must pause the job
// exactly like a gated shell command, and an approval must run it again.
func TestOperatorPausesWhenTheScreenGateStopsAGuiAction(t *testing.T) {
	chat := &scriptedChat{replies: []string{
		"COMMAND:\ngui click e35",
		"DONE:\nThe newsletter was sent.",
	}}
	op := NewOperator()
	rc := operatorContext(t, chat)

	// A fake gui: stopped until the grant file exists, as the real gate is
	// until a human's approval turns into a grant.
	bin := t.TempDir()
	grant := filepath.Join(t.TempDir(), "granted")
	script := "#!/bin/sh\nif [ -f " + grant + " ]; then echo 'Clicked button \"Send newsletter\" (e35).'; exit 0; fi\n" +
		"echo 'NEEDS_APPROVAL: Approval needed before I click button \"Send newsletter\" (e35) in \"Newsletter\": it sends something on your behalf. Nothing was clicked or sent.'\nexit 3\n"
	if err := os.WriteFile(filepath.Join(bin, "gui"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	res, _ := op.Run(rc, Subtask{Step: "send the newsletter", Type: "operator"})
	if res.Meta["needsApproval"] != true || res.Meta["action"] != "gui click e35" || res.Checkpoint == nil {
		t.Fatalf("result = %+v, want a paused approval for the click", res)
	}
	reason, _ := res.Meta["approvalReason"].(string)
	if !strings.Contains(reason, `click button "Send newsletter"`) || strings.Contains(reason, "Nothing was") {
		t.Errorf("approvalReason = %q, want the gate's description of the click, trimmed", reason)
	}
	if cmds, _ := res.Meta["commands"].([]string); len(cmds) != 0 {
		t.Fatalf("commands = %v - a stopped action was recorded as run", cmds)
	}

	if err := os.WriteFile(grant, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	rc.Resume = res.Checkpoint
	rc.HumanReply = "yes"
	rc.Approved = func(action string) bool { return action == "gui click e35" }
	res, _ = op.Run(rc, Subtask{Step: "send the newsletter", Type: "operator"})
	if !res.Verified || res.Output != "The newsletter was sent." {
		t.Fatalf("resumed result = %+v", res)
	}
	if !strings.Contains(chat.calls[1][len(chat.calls[1])-1].Content, "Clicked button") {
		t.Error("the approved click did not run on resume")
	}
}

func TestOperatorResumesWithoutTheCommandWhenNotApproved(t *testing.T) {
	chat := &scriptedChat{replies: []string{
		"COMMAND:\ngit push origin main",
		"DONE:\nOK, I left it unpushed.",
	}}
	op := NewOperator()
	rc := operatorContext(t, chat)
	res, _ := op.Run(rc, Subtask{Step: "push my branch", Type: "operator"})

	rc.Resume = res.Checkpoint
	rc.HumanReply = "no, open a PR instead later"
	res, _ = op.Run(rc, Subtask{Step: "push my branch", Type: "operator"})
	if !res.Verified {
		t.Fatalf("resumed result = %+v", res)
	}
	note := chat.calls[1][len(chat.calls[1])-1].Content
	if !strings.Contains(note, "did not approve") || !strings.Contains(note, "open a PR instead later") {
		t.Errorf("the refusal and the human's words did not reach the model:\n%s", note)
	}
}

func TestParseControl(t *testing.T) {
	cases := []struct{ in, action, value string }{
		{"COMMAND:\nuname -r", "command", "uname -r"},
		{"Sure. COMMAND: `df -h /`", "command", "df -h /"},
		{"COMMAND:\n```bash\nls -la\n```", "command", "ls -la"},
		{"**DONE:** 241", "done", "241"},
		{"Help: which folder?", "help", "which folder?"},
		{"DONE:\nIt needs help: nothing more.", "done", "It needs help: nothing more."},
		{"just 241", "invalid", "just 241"},
	}
	for _, c := range cases {
		action, value := parseControl(c.in)
		if action != c.action || value != c.value {
			t.Errorf("parseControl(%q) = (%q, %q), want (%q, %q)", c.in, action, value, c.action, c.value)
		}
	}
}

// A backgrounded server keeps the output pipe open; the command itself
// returned at once and must be reported as such, not hang to the timeout.
func TestRunShellDoesNotHangOnABackgroundedChild(t *testing.T) {
	start := time.Now()
	res := runShell(context.Background(), t.TempDir(), "sleep 30 & echo started", 20*time.Second)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("runShell took %s for a command that returns immediately", elapsed)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "started") {
		t.Errorf("result = %+v", res)
	}
}

func TestRunShellKillsATimedOutCommand(t *testing.T) {
	res := runShell(context.Background(), t.TempDir(), "echo before; sleep 30", 500*time.Millisecond)
	if !res.TimedOut || res.ExitCode != 124 || !strings.Contains(res.Output, "before") {
		t.Errorf("result = %+v, want a timed-out command with its partial output", res)
	}
}

// Nothing may read a terminal: an installer's "continue? [Y/n]" must fail
// fast on EOF instead of waiting forever.
func TestRunShellGivesCommandsNoStdin(t *testing.T) {
	res := runShell(context.Background(), t.TempDir(), "read -r answer; echo \"got=[$answer]\"", 5*time.Second)
	if !strings.Contains(res.Output, "got=[]") {
		t.Errorf("result = %+v, want read to see EOF", res)
	}
}

func TestResolveWritePathUsesTheWorkingDirectory(t *testing.T) {
	work := t.TempDir()
	rc := RunContext{WorkDir: work}
	got, err := resolveWritePath(rc, "notes/today.md")
	if err != nil || got != filepath.Join(work, "notes", "today.md") {
		t.Errorf("resolveWritePath = %q, %v", got, err)
	}
	if _, err := resolveWritePath(rc, "/etc/passwd"); err == nil {
		t.Error("resolveWritePath allowed /etc/passwd")
	}
}

// Measured live: a 3B model ran `ls -l /etc | wc -l`, saw exit 0 and "118",
// and replied HELP "It seems there was an error. Please try running the
// command again." One grounded push back gets the answer.
func TestOperatorPushesBackOnQueryRightAfterASuccess(t *testing.T) {
	chat := &scriptedChat{replies: []string{
		"COMMAND:\necho 118",
		"HELP:\nIt seems there was an error. Please try running the command again.",
		"DONE:\nThere are 118 entries in /etc.",
	}}
	res, _ := NewOperator().Run(operatorContext(t, chat), Subtask{Step: "how many files are in /etc?", Type: "operator"})
	if !res.Verified || res.Output != "There are 118 entries in /etc." {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(chat.calls[2][len(chat.calls[2])-1].Content, "Nothing failed") {
		t.Error("the push back did not tell the model its command succeeded")
	}
}

// A HELP that asks for something only a person can give is delivered at
// once, even straight after a success.
func TestOperatorDeliversAGenuineQueryAfterASuccess(t *testing.T) {
	chat := &scriptedChat{replies: []string{
		"COMMAND:\nls ~/projects",
		"HELP:\nThere are two projects, alpha and beta - which one should I delete?",
	}}
	res, _ := NewOperator().Run(operatorContext(t, chat), Subtask{Step: "delete the project", Type: "operator"})
	if res.Meta["needsUserInput"] != true || len(chat.calls) != 2 {
		t.Fatalf("result = %+v after %d calls, want the question delivered immediately", res, len(chat.calls))
	}
}

// Measured: HELP: NEEDS_TOOLS reached a human as a whole query.
func TestOperatorNeverDeliversTheChatSignal(t *testing.T) {
	chat := &scriptedChat{replies: []string{"HELP: NEEDS_TOOLS", "NEEDS_TOOLS", "DONE:\npong"}}
	res, _ := NewOperator().Run(operatorContext(t, chat), Subtask{Step: "ping - just reply pong", Type: "operator"})
	if !res.Verified || res.Output != "pong" {
		t.Fatalf("result = %+v", res)
	}
}

// Measured live: `... | xargs curl -s ...` failed on a desktop with no curl,
// and the person was paged "Install curl using sudo apt-get install curl".
// The loop tries another way first, and says what to use instead.
func TestOperatorPushesBackOnQueryRightAfterAFailure(t *testing.T) {
	chat := &scriptedChat{replies: []string{
		"COMMAND:\necho x | xargs no-such-program-decurix -s",
		"HELP:\nInstall no-such-program-decurix using `sudo apt-get install` before running the command.",
		"COMMAND:\necho found",
		"DONE:\nfound",
	}}
	res, _ := NewOperator().Run(operatorContext(t, chat), Subtask{Step: "fetch something", Type: "operator"})
	if !res.Verified || res.Output != "found" {
		t.Fatalf("result = %+v, want the second way to finish the task", res)
	}
	resultTurn := chat.calls[1][len(chat.calls[1])-1].Content
	if !strings.Contains(resultTurn, "`no-such-program-decurix` is not installed") {
		t.Errorf("the failed command's result did not say the program is missing:\n%s", resultTurn)
	}
	nudge := chat.calls[2][len(chat.calls[2])-1].Content
	if !strings.Contains(nudge, "did not work") || !strings.Contains(nudge, "never ask them to run or install") {
		t.Errorf("push back = %q", nudge)
	}
}

// Twice is enough: a third HELP after failures goes to the person.
func TestOperatorDeliversAQueryAfterRepeatedFailures(t *testing.T) {
	chat := &scriptedChat{replies: []string{
		"COMMAND:\nfalse", "HELP:\nit failed",
		"COMMAND:\nexit 2", "HELP:\nit failed again",
		"COMMAND:\nexit 3", "HELP:\nstill failing",
	}}
	res, _ := NewOperator().Run(operatorContext(t, chat), Subtask{Step: "do the thing", Type: "operator"})
	if res.Meta["needsUserInput"] != true || res.Output != "still failing" {
		t.Fatalf("result = %+v after %d calls, want the third query delivered", res, len(chat.calls))
	}
}

// Approval is never argued with, failure or not.
func TestOperatorDeliversAnApprovalQueryAfterAFailure(t *testing.T) {
	chat := &scriptedChat{replies: []string{"COMMAND:\nfalse", "HELP:\nMay I have your permission to delete the old logs?"}}
	res, _ := NewOperator().Run(operatorContext(t, chat), Subtask{Step: "tidy the logs", Type: "operator"})
	if res.Meta["needsUserInput"] != true || len(chat.calls) != 2 {
		t.Fatalf("result = %+v after %d calls, want the approval delivered immediately", res, len(chat.calls))
	}
}

func TestMissingProgramHintNamesOnlyAProgramTheCommandRan(t *testing.T) {
	if hint := missingProgramHint("xargs: curl: No such file or directory", "echo q | xargs curl -s https://x"); !strings.Contains(hint, "python3") {
		t.Errorf("curl hint = %q", hint)
	}
	if hint := missingProgramHint("bash: line 1: jq: command not found", "cat a.json | jq ."); !strings.Contains(hint, "`jq` is not installed") {
		t.Errorf("jq hint = %q", hint)
	}
	// A file the program was given is not a missing program.
	if hint := missingProgramHint("cat: notes.txt: No such file or directory", "cat notes.txt"); hint != "" {
		t.Errorf("a missing file was taken for a missing program: %q", hint)
	}
}

// Measured live: python3 with a missing module failed twice, and the model
// answered with a header nothing had printed. A failed repeat does not invite
// an answer, and a DONE with no successful command is pushed back once.
func TestOperatorDoesNotTakeAnAnswerNothingShowed(t *testing.T) {
	chat := &scriptedChat{replies: []string{
		"COMMAND:\npython3 -c \"import no_such_module_decurix\"",
		"COMMAND:\npython3 -c \"import no_such_module_decurix\"",
		"DONE:\ntext/html; charset=utf-8",
		"COMMAND:\necho text/html",
		"DONE:\ntext/html",
	}}
	res, _ := NewOperator().Run(operatorContext(t, chat), Subtask{Step: "what content-type does example.com send?", Type: "operator"})
	if !res.Verified || res.Output != "text/html" {
		t.Fatalf("result = %+v, want the answer the command printed", res)
	}
	repeat := chat.calls[2][len(chat.calls[2])-1].Content
	if !strings.Contains(repeat, "FAILED") || strings.Contains(repeat, "reply now with DONE") || !strings.Contains(repeat, "`no_such_module_decurix` is not installed") {
		t.Errorf("repeat nudge = %q", repeat)
	}
	if pushback := chat.calls[3][len(chat.calls[3])-1].Content; !strings.Contains(pushback, "Do not answer from memory") {
		t.Errorf("an answer after nothing but failures was not pushed back: %q", pushback)
	}
}

// A failure can be the answer: said again, quoting the error, it is taken.
func TestOperatorTakesAFailureThatIsTheAnswer(t *testing.T) {
	chat := &scriptedChat{replies: []string{
		"COMMAND:\nls ~/work/no-such-report.txt",
		"DONE:\nThere is no such file.",
		"DONE:\nThere is no such file: ls says \"No such file or directory\".",
	}}
	res, _ := NewOperator().Run(operatorContext(t, chat), Subtask{Step: "is there a report in ~/work?", Type: "operator"})
	if !res.Verified || !strings.Contains(res.Output, "No such file or directory") {
		t.Fatalf("result = %+v after %d calls", res, len(chat.calls))
	}
}
