package skills

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"thebot/internal/hostshell"
	"thebot/internal/workspace"
)

func newTestWorkspace(t *testing.T) *workspace.Workspace {
	t.Helper()
	ws, err := workspace.New(t.TempDir())
	if err != nil {
		t.Fatalf("workspace.New() error = %v", err)
	}
	return ws
}

// The planner is instructed to emit "run the command: X", so this
// phrasing MUST work - it failed every time until now, which made every
// planned terminal step dead on arrival despite a correct-looking plan.
func TestExtractCommandAcceptsPlainRunPhrasing(t *testing.T) {
	cases := map[string]string{
		"run the command: python3 avl_tree.py": "python3 avl_tree.py",
		"run: go test ./...":                   "go test ./...",
		"execute the command ls -la":           "ls -la",
		"run the command: pwd":                 "pwd",
		"run ./build.sh":                       "./build.sh",
		"run the command: python3 x.py.":       "python3 x.py",
	}
	for step, want := range cases {
		got, source := extractCommand(step)
		if got != want {
			t.Errorf("extractCommand(%q) = %q, want %q", step, got, want)
		}
		if got != "" && source == "" {
			t.Errorf("extractCommand(%q) returned a command with no source", step)
		}
	}
}

// The live bug this pins: "run the command: pip install -r
// requirements.txt (assuming a requirements.txt file exists)" ran the
// ENTIRE parenthetical as part of the literal shell command and failed
// with exit 2 - "(assuming a requirements.txt file exists)" is the
// model's own narrated caveat, not shell syntax.
func TestExtractCommandStripsTrailingNarratedCaveat(t *testing.T) {
	cases := map[string]string{
		"run the command: pip install -r requirements.txt (assuming a requirements.txt file exists)": "pip install -r requirements.txt",
		"run the command: go build ./... (if this fails, check go.mod)":                              "go build ./...",
		"run the command: npm test (note this may take a while)":                                     "npm test",
	}
	for step, want := range cases {
		got, _ := extractCommand(step)
		if got != want {
			t.Errorf("extractCommand(%q) = %q, want %q", step, got, want)
		}
	}
}

// A command that genuinely uses parens as real shell syntax (a subshell)
// must not be mangled - only an English-narrating parenthetical is
// stripped.
func TestExtractCommandPreservesRealShellParens(t *testing.T) {
	got, _ := extractCommand("run the command: echo $(date)")
	if got != "echo $(date)" {
		t.Errorf("extractCommand() = %q, want the real subshell preserved", got)
	}
}

// Backticks stay authoritative - the looser rule must not override an
// explicitly delimited command.
func TestExtractCommandPrefersBackticks(t *testing.T) {
	got, source := extractCommand("run the command: `echo hi` please")
	if got != "echo hi" {
		t.Errorf("extractCommand() = %q, want the backticked command", got)
	}
	if !strings.Contains(source, "backticks") {
		t.Errorf("source = %q, want it to credit the backticks", source)
	}
}

// The live bug this pins - found via a 100+ case terminal battery:
// strings.Trim(s, `"'`) strips independently from EACH end, so a command
// ending in a legitimately quoted last argument ("echo 'has $pecial &
// chars!'", "mv a 'b c.txt'") had its trailing quote silently removed
// even though nothing matched on the leading end, producing an
// unterminated-quote command that failed at the real shell with a syntax
// error (exit 2). Only a genuinely symmetric wrap ("the whole command is
// one quoted string") may ever be stripped.
func TestExtractCommandOnlyStripsSymmetricWrappingQuotes(t *testing.T) {
	cases := map[string]string{
		`run the command: echo "literal $NOTAVAR here"`:           `echo "literal $NOTAVAR here"`,
		`run the command: echo ''`:                                `echo ''`,
		`run the command: echo "outer 'inner' outer"`:             `echo "outer 'inner' outer"`,
		`run the command: mv plain.txt 'renamed with spaces.txt'`: `mv plain.txt 'renamed with spaces.txt'`,
		`run the command: echo 'has $pecial & chars!'`:            `echo 'has $pecial & chars!'`,
		`run the command: "ls -la"`:                               `ls -la`, // a genuine whole-command wrap still strips
		"run the command: `git status`":                           `git status`,
	}
	for step, want := range cases {
		got, _ := extractCommand(step)
		if got != want {
			t.Errorf("extractCommand(%q) = %q, want %q", step, got, want)
		}
	}
}

// The live bug this pins: a failure-reasoning retry step routinely drops
// the "run the command:" framing entirely - "run the command: pip
// install X" becomes just "python3 -m venv ... && pip install X" as the
// RETRY_STEP - and extractCommand found nothing for it, so the retry
// silently never ran the real command at all.
func TestExtractCommandAcceptsBareShellSyntax(t *testing.T) {
	cases := map[string]string{
		"python3 -m venv /home/headless/ai/myenv && source /home/headless/ai/myenv/bin/activate && pip install -r requirements.txt": "python3 -m venv /home/headless/ai/myenv && source /home/headless/ai/myenv/bin/activate && pip install -r requirements.txt",
		"go test ./... -v":        "go test ./... -v",
		"`echo already-backtick`": "echo already-backtick",
	}
	for step, want := range cases {
		got, source := extractCommand(step)
		if got != want {
			t.Errorf("extractCommand(%q) = %q, want %q", step, got, want)
		}
		if got != "" && source == "" {
			t.Errorf("extractCommand(%q) returned a command with no source", step)
		}
	}
}

// A known, accepted boundary: a short bare command with no distinguishing
// shell-syntax marker at all ("pip install flask") is indistinguishable
// from ordinary prose at this strict gate and is deliberately rejected -
// the safe choice, since loosening it risks executing a sentence. Only
// compound/flagged/pathed commands recover via the bare fallback; a
// plain one still needs "run the command:" framing or backticks.
func TestExtractCommandBareFallbackRejectsAmbiguousShortCommands(t *testing.T) {
	if got, _ := extractCommand("pip install flask"); got != "" {
		t.Errorf("extractCommand(%q) = %q, want empty (documented boundary - no shell-syntax marker to distinguish it from prose)", "pip install flask", got)
	}
}

// The dangerous direction, and the exact live regression this pins: a
// bare fallback must NEVER accept plain prose just because it's 2+ words
// with no leading pronoun - that already executed "please do something
// vague with no backticks" as a real shell command the first time this
// was tried ("bash: line 1: please: command not found").
func TestExtractCommandBareFallbackRejectsPlainProse(t *testing.T) {
	for _, step := range []string{
		"please do something vague with no backticks",
		"go check whether the file exists and let me know",
		"try running the tests again and see what happens",
		"figure out why it failed and fix it",
	} {
		if got, _ := extractCommand(step); got != "" {
			t.Errorf("extractCommand(%q) = %q, want empty - the bare fallback must not accept plain prose as a shell command", step, got)
		}
	}
}

// The dangerous direction: prose must never reach a shell.
func TestExtractCommandRejectsProse(t *testing.T) {
	for _, step := range []string{
		"run it again",
		"run that",
		"execute the script now",
		"please use the terminal",
		"run this",
	} {
		if got, _ := extractCommand(step); got != "" {
			t.Errorf("extractCommand(%q) = %q, want empty - prose must never be executed", step, got)
		}
	}
}

func TestTerminalMatches(t *testing.T) {
	tm := NewTerminal()
	if !tm.Matches(Subtask{Step: "anything", Type: "terminal"}) {
		t.Error("Matches() = false for Type=terminal, want true")
	}
	if tm.Matches(Subtask{Step: "anything", Type: "web"}) {
		t.Error("Matches() = true for Type=web, want false")
	}
}

func TestTerminalRunSuccess(t *testing.T) {
	if runtime.GOOS == "windows" {
		// exec.Command shells out to "bash" - relies on Git Bash being on
		// PATH, which it is in this dev environment, so this still runs
		// for real rather than being skipped.
	}
	ws := newTestWorkspace(t)
	var logs []string
	rc := RunContext{Ctx: context.Background(), OnLog: func(s string) { logs = append(logs, s) }, Workspace: ws}

	result, err := NewTerminal().Run(rc, Subtask{Step: "run this command: `echo real-output-123`", Type: "terminal"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !result.Verified {
		t.Errorf("Verified = false, want true. Output: %s", result.Output)
	}
	if !strings.Contains(result.Output, "real-output-123") {
		t.Errorf("Output does not contain real command output: %s", result.Output)
	}
	if result.Meta["exitCode"] != 0 {
		t.Errorf("Meta[exitCode] = %v, want 0", result.Meta["exitCode"])
	}
	if len(logs) == 0 {
		t.Error("no log lines were emitted via OnLog")
	}
}

// The live bug this pins - found via a 100+ case terminal battery: `grep
// pattern file` finding no match, or plain `false`, are real, honest,
// COMPLETE results - not a failure of the agent's job, which was simply
// to run the command and report what actually happened. Treating them as
// Verified:false triggered a full LLM failure-reasoning-and-retry cycle
// that quietly substituted an unrelated command that happened to exit 0
// ("false" silently became "ls -l" in one real run), discarding what was
// actually asked and misreporting a perfectly successful grep as broken.
func TestTerminalTreatsOrdinaryNonzeroExitAsARealVerifiedResult(t *testing.T) {
	ws := newTestWorkspace(t)
	if err := os.WriteFile(filepath.Join(ws.Root(), "haystack.txt"), []byte("apple\nbanana\n"), 0o644); err != nil {
		t.Fatalf("could not write fixture: %v", err)
	}
	rc := RunContext{Ctx: context.Background(), OnLog: func(string) {}, Workspace: ws}

	steps := []string{
		"run the command: `grep zzz_no_such_pattern haystack.txt`",
		"run the command: `false`",
		"run the command: `test -f definitely_missing_xyz.txt`",
	}
	if hostshell.Windows {
		// The same honest negatives from Windows' own programs: a search
		// that matches nothing, and a program that exits 1 without a word.
		steps = []string{
			"run the command: `findstr zzz_no_such_pattern haystack.txt`",
			"run the command: `cmd /c exit 1`",
		}
	}
	for _, step := range steps {
		result, err := NewTerminal().Run(rc, Subtask{Step: step, Type: "terminal"})
		if err != nil {
			t.Fatalf("Run(%q) error = %v", step, err)
		}
		if !result.Verified {
			t.Errorf("Run(%q).Verified = false, want true - this is a real, honest, complete result, not an execution failure. Output: %s", step, result.Output)
		}
		if !strings.Contains(result.Output, "Exit code:") {
			t.Errorf("Run(%q).Output = %q, want the real exit code still reported", step, result.Output)
		}
	}
}

// The two codes bash itself reserves for "this could not even be run" -
// not found (127), found but not executable (126) - must stay real
// failures, worth reasoning about and retrying.
func TestTerminalStillTreatsCommandNotFoundAndNotExecutableAsRealFailures(t *testing.T) {
	ws := newTestWorkspace(t)
	scriptPath := filepath.Join(ws.Root(), "not_executable.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\necho hi\n"), 0o644); err != nil { // deliberately no +x
		t.Fatalf("could not write fixture: %v", err)
	}
	rc := RunContext{Ctx: context.Background(), OnLog: func(string) {}, Workspace: ws}

	notFound, err := NewTerminal().Run(rc, Subtask{Step: "run the command: `this-command-does-not-exist-xyz`", Type: "terminal"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if notFound.Verified {
		t.Error("command-not-found (exit 127) reported Verified=true, want false - this is a genuine execution failure")
	}
	// Live: `curl` missing led to `sudo apt-get install curl`, then to a
	// question for the person. The operator gets the goal instead, told
	// the program is not there.
	if notFound.Meta["reroute"] != "operator" {
		t.Errorf("command-not-found Meta = %+v, want a reroute to the operator", notFound.Meta)
	}
	if note, _ := notFound.Meta["rerouteNote"].(string); !strings.Contains(note, "`this-command-does-not-exist-xyz` is not "+forShell("installed", "a command on this computer")) {
		t.Errorf("reroute note = %q, want it to name the missing program", note)
	}

	if runtime.GOOS == "windows" {
		// Git Bash on Windows doesn't enforce the Unix exec bit the way a
		// real Linux target does (NTFS has no such permission bit, and
		// Git Bash's exec layer runs a script via its shebang regardless)
		// - this sub-case is only meaningfully testable on the real
		// deployment target and is verified there separately.
		return
	}
	notExecutable, err := NewTerminal().Run(rc, Subtask{Step: "run the command: `./not_executable.sh`", Type: "terminal"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if notExecutable.Verified {
		t.Error("not-executable (exit 126) reported Verified=true, want false - this is a genuine execution failure")
	}
}

// The regression the exit-code heuristic itself needed pinning against,
// found by testing the fix live: exit code alone can't distinguish "ran
// fine, told us something" from "genuinely failed" - grep's no-match and
// pip failing on an externally-managed environment both exit 1. A
// blanket "only 126/127 are real failures" rule (the first version of
// this fix) would have reported a failed pip install as Verified:true,
// since 1 isn't 126 or 127 - masking a real failure as a success. STDERR
// content is the real signal: a tool that completes normally and merely
// reports a negative result writes nothing there; a tool that genuinely
// couldn't do what was asked does.
func TestTerminalTreatsNonzeroExitWithRealStderrAsARealFailure(t *testing.T) {
	ws := newTestWorkspace(t)
	rc := RunContext{Ctx: context.Background(), OnLog: func(string) {}, Workspace: ws}

	// A script that exits 1 (not 126/127) but writes a real, substantive
	// error message - exactly pip's shape (exit 1, real stderr block).
	name, script := "fails_with_stderr.sh", "#!/bin/sh\necho 'error: something genuinely went wrong' >&2\nexit 1\n"
	if hostshell.Windows {
		name, script = "fails_with_stderr.cmd", "@echo off\r\necho error: something genuinely went wrong 1>&2\r\nexit /b 1\r\n"
	}
	if err := os.WriteFile(filepath.Join(ws.Root(), name), []byte(script), 0o755); err != nil {
		t.Fatalf("could not write fixture: %v", err)
	}

	result, err := NewTerminal().Run(rc, Subtask{Step: "run the command: `./" + name + "`", Type: "terminal"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Verified {
		t.Error("Verified = true for a nonzero exit with real stderr content, want false - this is a genuine failure, not an honest negative result")
	}
	if !strings.Contains(result.Output, "something genuinely went wrong") {
		t.Errorf("Output = %q, want the real stderr content reported", result.Output)
	}
}

// A gui command given by name ("run the command: `gui click ...`") that the
// screen gate stops is a pause for approval, as it is in the operator's loop:
// found live, it was reported as the command's ordinary result and the
// person's yes had nothing to resume. Approved, the same command runs again.
func TestTerminalPausesWhenTheScreenGateStopsAGuiCommand(t *testing.T) {
	grant := filepath.Join(t.TempDir(), "granted")
	stubWorkerTools(t,
		"if [ -f "+grant+" ]; then echo 'Clicked button \"Send message\" (e85).'; exit 0; fi\n"+
			"echo 'NEEDS_APPROVAL: Approval needed before I click button \"Send message\" (e85) in \"Message form\": it sends something on your behalf. Nothing was clicked or sent.'\nexit 3\n",
		"",
		"if exist \""+grant+"\" goto granted\r\n"+
			"echo NEEDS_APPROVAL: Approval needed before I click button \"Send message\" (e85) in \"Message form\": it sends something on your behalf. Nothing was clicked or sent.\r\nexit /b 3\r\n"+
			":granted\r\necho Clicked button \"Send message\" (e85).\r\nexit /b 0\r\n")

	ws := newTestWorkspace(t)
	rc := RunContext{Ctx: context.Background(), OnLog: func(string) {}, Workspace: ws}
	step := Subtask{Step: "run the command: `gui click e85`", Type: "terminal"}

	res, err := NewTerminal().Run(rc, step)
	if err != nil {
		t.Fatal(err)
	}
	if res.Meta["needsApproval"] != true || res.Meta["action"] != "gui click e85" || res.Verified {
		t.Fatalf("result = %+v, want a paused approval for the click", res)
	}
	if reason, _ := res.Meta["approvalReason"].(string); !strings.Contains(reason, `click button "Send message"`) {
		t.Errorf("approvalReason = %q, want the gate's own description", reason)
	}

	// The person's yes became a grant (the desktop agent does that); the
	// same command, run again, goes through.
	if err := os.WriteFile(grant, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	res, _ = NewTerminal().Run(rc, step)
	if !res.Verified || !strings.Contains(res.Output, "Clicked button") {
		t.Fatalf("after approval, result = %+v", res)
	}
}

func TestTerminalRunFailure(t *testing.T) {
	ws := newTestWorkspace(t)
	rc := RunContext{Ctx: context.Background(), OnLog: func(string) {}, Workspace: ws}

	result, err := NewTerminal().Run(rc, Subtask{Step: "run this command: `this-command-does-not-exist-xyz`", Type: "terminal"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Verified {
		t.Errorf("Verified = true for a command that doesn't exist, want false. Output: %s", result.Output)
	}
	if result.Meta["exitCode"] == 0 {
		t.Errorf("Meta[exitCode] = 0 for a failed command, want nonzero")
	}
}

func TestTerminalRunNoCommandFound(t *testing.T) {
	ws := newTestWorkspace(t)
	rc := RunContext{Ctx: context.Background(), OnLog: func(string) {}, Workspace: ws}

	result, err := NewTerminal().Run(rc, Subtask{Step: "please do something vague with no backticks", Type: "terminal"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Verified {
		t.Error("Verified = true with no extractable command, want false")
	}
	if !strings.Contains(result.Output, "Could not determine") {
		t.Errorf("Output = %q, expected a clear 'could not determine command' message", result.Output)
	}
}

func TestTerminalRunsInsideWorkspaceRoot(t *testing.T) {
	ws := newTestWorkspace(t)
	rc := RunContext{Ctx: context.Background(), OnLog: func(string) {}, Workspace: ws}

	result, err := NewTerminal().Run(rc, Subtask{Step: "run this command: `pwd`", Type: "terminal"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !result.Verified {
		t.Fatalf("Run() not verified: %s", result.Output)
	}
	// pwd's real output must reflect the real workspace root, not wherever
	// the test binary happens to run from.
	if !strings.Contains(strings.ToLower(result.Output), strings.ToLower(ws.Root())) {
		t.Errorf("pwd output %q does not reference workspace root %q", result.Output, ws.Root())
	}
}

// Live: the whole sentence ran as the command and failed with exit 127.
func TestExtractCommandLeavesWhatToReportForTheReply(t *testing.T) {
	cases := map[string]string{
		"run curl -sI https://example.com in the terminal and tell me the content-type header": "curl -sI https://example.com",
		"run `df -h` and tell me how much is free":                                             "df -h",
		"run uname -r from the shell":                                                          "uname -r",
		"execute ls -la ~/work, then show me the biggest file":                                 "ls -la ~/work",
		// What is part of a command stays.
		"run the command: grep -r \"tell me\" notes/": "grep -r \"tell me\" notes/",
		"run echo hello && echo world":                "echo hello && echo world",
	}
	for step, want := range cases {
		if got, _ := extractCommand(step); got != want {
			t.Errorf("extractCommand(%q) = %q, want %q", step, got, want)
		}
	}
}
