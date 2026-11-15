// Tests that a failure a pipe's exit code hid is the result: in the operator's shell and in the terminal skill.
package skills

import (
	"context"
	"strings"
	"testing"
	"time"
)

// tracked reports whether this host's bash reports pipe statuses (Git Bash and Linux bash do).
func tracked(t *testing.T) {
	t.Helper()
	if r := runShell(context.Background(), t.TempDir(), "false | cat", 10*time.Second); r.ExitCode == 0 {
		t.Skip("this bash does not report pipe statuses")
	}
}

// `failing_check | tail -5` exited 0 and was taken as a success: now its exit code is the failing program's and the
// output says why.
func TestRunShellReportsAFailureThePipeHid(t *testing.T) {
	tracked(t)
	dir := t.TempDir()
	r := runShell(context.Background(), dir, "ls ./no-such-file-here | tail -5", 10*time.Second)
	if r.ExitCode == 0 {
		t.Fatalf("exit code 0 for a failed ls behind tail: %q", r.Output)
	}
	if !strings.Contains(r.Output, "did NOT succeed") || !strings.Contains(r.Output, "`ls`") {
		t.Fatalf("output does not say which program failed: %q", r.Output)
	}
}

// A pipe that ended early by design, or a search that found nothing, is not a failure.
func TestRunShellKeepsBenignPipesAsTheyWere(t *testing.T) {
	tracked(t)
	dir := t.TempDir()
	for _, line := range []string{
		"yes | head -1",                        // SIGPIPE
		"grep -c zz /dev/null | cat",           // grep's exit 1 is its answer
		"echo a | tr a b",                      // all succeed
		"for i in 1 2; do echo $i; done | cat", // a loop piped
	} {
		r := runShell(context.Background(), dir, line, 10*time.Second)
		if r.ExitCode != 0 || strings.Contains(r.Output, "exit status:") {
			t.Errorf("%q: exit %d, output %q", line, r.ExitCode, r.Output)
		}
	}
	// The pipe's own failure stays as it was: visible already, nothing added.
	r := runShell(context.Background(), dir, "echo a | false", 10*time.Second)
	if r.ExitCode != 1 || strings.Contains(r.Output, "exit status:") {
		t.Errorf("visible failure changed: exit %d, %q", r.ExitCode, r.Output)
	}
}

// The terminal skill does not verify a pipe whose first program failed.
func TestTerminalDoesNotVerifyAFailureThePipeHid(t *testing.T) {
	tracked(t)
	rc := RunContext{Ctx: context.Background(), OnLog: func(string) {}, Workspace: newTestWorkspace(t)}
	res, err := NewTerminal().Run(rc, Subtask{Step: "run this command: `ls ./no-such-file-here | tail -5`", Type: "terminal"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Verified {
		t.Fatalf("verified a failed ls behind tail: %s", res.Output)
	}
	if res.Meta["exitCode"] == 0 || res.Meta["fatal"] != true {
		t.Fatalf("meta = %v", res.Meta)
	}
	ok, err := NewTerminal().Run(rc, Subtask{Step: "run this command: `yes | head -1`", Type: "terminal"})
	if err != nil || !ok.Verified {
		t.Fatalf("a pipe ended by head was not verified: %v %s", err, ok.Output)
	}
}

// A command named after a colon is the command, never the words describing it (found live 2026-11-15: "exactly this
// shell command once" was handed to bash).
func TestACommandNamedAfterAColonIsTheCommand(t *testing.T) {
	for step, want := range map[string]string{
		"Run exactly this shell command once and tell me whether it succeeded: ls /no/such/folder | tail -1": "ls /no/such/folder | tail -1",
		"please run this command for me: python3 report.py --month 10":                                       "python3 report.py --month 10",
		"execute the following script now: ./build.sh and tell me what it printed":                           "./build.sh",
		"run the command: python3 avl_tree.py":                                                               "python3 avl_tree.py",
		"run: go test ./...":                                                                                 "go test ./...",
	} {
		if got, _ := extractCommand(step); got != want {
			t.Errorf("extractCommand(%q) = %q, want %q", step, got, want)
		}
	}
}

// Programs are named only when the line ends in the pipeline; otherwise by position.
func TestHiddenPipeFailureNaming(t *testing.T) {
	f := hiddenPipeFailure("cd x && pytest -q | tail -5", []int{1, 0}, 0)
	if f == nil || f.Program != "pytest" || f.Position != 1 || f.Of != 2 || f.Status != 1 {
		t.Fatalf("named: %+v", f)
	}
	f = hiddenPipeFailure("for i in 1 2; do false | true; done", []int{1, 0}, 0)
	if f == nil || f.Program != "" || !strings.Contains(f.note(), "program 1 of 2") {
		t.Fatalf("by position: %+v", f)
	}
	if hiddenPipeFailure("grep x f | wc -l", []int{1, 0}, 0) != nil {
		t.Fatal("grep finding nothing was called a failure")
	}
	if hiddenPipeFailure("env FOO=1 /usr/bin/diff a b | head", []int{1, 0}, 0) != nil {
		t.Fatal("diff's answer behind wrappers was called a failure")
	}
	if hiddenPipeFailure("false | true", nil, 0) != nil {
		t.Fatal("unknown statuses produced a failure")
	}
}
