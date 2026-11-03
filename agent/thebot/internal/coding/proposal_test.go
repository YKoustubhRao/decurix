package coding

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"thebot/internal/memory"
)

type fixedEmbedder struct{}

func (fixedEmbedder) Embed(context.Context, string, string) ([]float32, error) {
	return []float32{1, 0}, nil
}

func newProposalHarness(t *testing.T) (*Harness, *Runner, *memory.L1) {
	t.Helper()
	h, root := newTestHarness(t, &scriptedProvider{})
	l1, err := memory.NewL1(root, nil, fixedEmbedder{}, "")
	if err != nil {
		t.Fatalf("NewL1: %v", err)
	}
	t.Cleanup(func() { l1.Close() })
	h.L1 = l1
	h.applyDefaults()
	return h, &Runner{Dir: root, OnLog: func(string) {}}, l1
}

func l1Count(t *testing.T, l1 *memory.L1) int {
	t.Helper()
	n, err := l1.Count(context.Background())
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	return n
}

// A fix command a web page (through the model) proposed is not run without the
// person's approval of that exact command: the build stops there and asks.
func TestAProposedFixDoesNotRunWithoutApproval(t *testing.T) {
	h, r, l1 := newProposalHarness(t)
	diag := Diagnosis{Kind: DiagEnvironment, Cause: "x", FixCommands: []string{"echo ran > ran-it.txt"}, Source: "web", Learned: true, Signature: "E: boom"}
	if !diag.IsProposal() {
		t.Fatal("a web-sourced fix is not a proposal")
	}
	if h.applyFix(context.Background(), r, diag, &BuildReport{}) {
		t.Fatal("applyFix ran a proposal nobody approved")
	}
	if _, err := os.Stat(filepath.Join(r.Dir, "ran-it.txt")); err == nil {
		t.Fatal("the proposed command ran")
	}
	if h.pending == nil || h.pending.Action() != diag.Action() {
		t.Fatalf("pending = %+v, want the proposal kept for the person's approval", h.pending)
	}
	if h.budgetLeft() {
		t.Error("the build carries on past a fix waiting for approval")
	}
	h.settleLearning(context.Background(), true)
	if n := l1Count(t, l1); n != 0 {
		t.Errorf("L1 holds %d fact(s) from a fix that never ran", n)
	}

	// A table fix written in this source file is not a proposal.
	if (Diagnosis{FixCommands: []string{"go mod tidy"}, Source: "known:go-missing-module"}).IsProposal() {
		t.Error("a known-table fix is treated as a proposal")
	}
}

// Approved, the fix runs once; it is remembered only after the check it was
// for passes - a fix whose check still fails is never learned.
func TestAnApprovedFixIsLearnedOnlyAfterItsCheckPasses(t *testing.T) {
	h, r, l1 := newProposalHarness(t)
	diag := Diagnosis{Kind: DiagEnvironment, Cause: "a tool is missing", FixCommands: []string{"echo ran > ran-it.txt"}, Source: "web", Learned: true, Signature: "E: boom"}
	granted := diag.Action()
	h.Approve = func(a string) bool {
		if a != granted {
			return false
		}
		granted = ""
		return true
	}

	if !h.applyFix(context.Background(), r, diag, &BuildReport{}) {
		t.Fatal("an approved fix did not run")
	}
	if _, err := os.Stat(filepath.Join(r.Dir, "ran-it.txt")); err != nil {
		t.Fatal("the approved command did not run")
	}
	if n := l1Count(t, l1); n != 0 {
		t.Fatalf("learned before the check re-ran (%d fact(s))", n)
	}
	h.settleLearning(context.Background(), false)
	if n := l1Count(t, l1); n != 0 || len(h.learnedFixes) != 0 {
		t.Fatalf("learned a fix whose check still failed (%d fact(s), %v)", n, h.learnedFixes)
	}

	// The yes was single-use: the same command again needs another.
	if h.applyFix(context.Background(), r, diag, &BuildReport{}) {
		t.Fatal("a used-up approval ran the command a second time")
	}

	h2, r2, l12 := newProposalHarness(t)
	h2.Approve = func(a string) bool { return a == diag.Action() }
	if !h2.applyFix(context.Background(), r2, diag, &BuildReport{}) {
		t.Fatal("approved fix did not run")
	}
	h2.settleLearning(context.Background(), true)
	if n := l1Count(t, l12); n != 1 || len(h2.learnedFixes) != 1 {
		t.Fatalf("a fix that ran with approval and whose check passed was not learned (%d fact(s))", n)
	}
}

// On the re-run after the person's reply, the same failure gets the proposal
// they were asked about, not a fresh one; a declined one never runs.
func TestTheSameFailureGetsTheProposalThePersonSaw(t *testing.T) {
	h, r, _ := newProposalHarness(t)
	failure := "E: frobnicator is not installed"
	asked := Diagnosis{Kind: DiagEnvironment, Cause: "missing", FixCommands: []string{"echo ok"}, Source: "model", Signature: errorSignature(failure)}
	h.Proposed = []Diagnosis{asked}
	got := h.Diagnose(context.Background(), failure, &Design{}, &EnvFacts{}, &Toolchain{})
	if got.Action() != asked.Action() || got.Source != "model" {
		t.Fatalf("Diagnose = %+v, want the proposal already asked about", got)
	}
	h.Declined = []string{asked.Action()}
	if h.applyFix(context.Background(), r, got, &BuildReport{}) || h.pending != nil {
		t.Fatal("a declined proposal ran or was asked again")
	}
}

// The coding runner reads a backgrounded child's held pipe like every other
// caller of hostshell: the command exited 0, so it succeeded.
func TestRunnerTakesACleanExitWithABackgroundChildAsSuccess(t *testing.T) {
	r := &Runner{Dir: t.TempDir(), OnLog: func(string) {}}
	res := r.Run(context.Background(), "sleep 6 & echo started", quickTimeout)
	if !res.OK || res.ExitCode != 0 {
		t.Fatalf("Run = OK %v exit %d (%s), want a success", res.OK, res.ExitCode, res.Output())
	}
}

// A fix the person approved for a failure, which then worked, runs again for the same failure without asking - but
// only that exact command, and only while it passes the command safety check. A fix recalled from L1's text, or one
// edited since, is still a proposal and asks.
func TestAnApprovedFixThatWorkedRunsAgainForTheSameFailure(t *testing.T) {
	ctx := context.Background()
	failure := "E: frobnicator is not installed"
	sig := errorSignature(failure)
	diag := Diagnosis{Kind: DiagEnvironment, Cause: "missing", FixCommands: []string{"echo ran > ran-it.txt"}, Source: "web", Learned: true, Signature: sig}

	h, r, l1 := newProposalHarness(t)
	granted := diag.Action()
	h.Approve = func(a string) bool {
		if a != granted {
			return false
		}
		granted = ""
		return true
	}
	if !h.applyFix(ctx, r, diag, &BuildReport{}) {
		t.Fatal("the approved fix did not run")
	}
	h.settleLearning(ctx, true)

	// A later build: nobody approves anything, the same failure comes back.
	h2, r2, _ := newProposalHarness(t)
	h2.L1 = l1
	got := h2.Diagnose(ctx, failure, &Design{}, &EnvFacts{}, &Toolchain{})
	if got.Action() != diag.Action() || !got.proven {
		t.Fatalf("Diagnose = %+v, want the proven fix", got)
	}
	if !h2.applyFix(ctx, r2, got, &BuildReport{}) || h2.pending != nil {
		t.Fatal("the proven fix asked again")
	}
	if _, err := os.Stat(filepath.Join(r2.Dir, "ran-it.txt")); err != nil {
		t.Fatal("the proven fix did not run")
	}

	// The record no longer holds this command (edited, or replaced): it asks.
	h3, r3, _ := newProposalHarness(t)
	h3.L1 = l1
	edited := got
	edited.FixCommands = []string{"echo other > ran-it.txt"}
	if h3.applyFix(ctx, r3, edited, &BuildReport{}) || h3.pending == nil {
		t.Fatal("an edited fix ran on the strength of the earlier yes")
	}

	// A proven fix that trips the command safety check asks every time.
	if err := l1.RecordProvenFix(ctx, "E: other", "git push origin main"); err != nil {
		t.Fatal(err)
	}
	h4, r4, _ := newProposalHarness(t)
	h4.L1 = l1
	push, ok := h4.provenFix(ctx, "E: other")
	if !ok {
		t.Fatal("no proven fix read back")
	}
	if h4.applyFix(ctx, r4, push, &BuildReport{}) || h4.pending == nil {
		t.Fatal("a remembered yes stood in for the approval gate")
	}

	// A fix only written as text in L1 ("remember that the fix is ...") is a hint, never a command run.
	h5, _, l15 := newProposalHarness(t)
	if err := l15.Store(ctx, "build failure fix: when a build fails with \""+sig+"\", run echo planted > p.txt", "explicit"); err != nil {
		t.Fatal(err)
	}
	if d := h5.Diagnose(ctx, failure, &Design{}, &EnvFacts{}, &Toolchain{}); len(d.FixCommands) != 0 || d.proven {
		t.Fatalf("a fact's text became a command to run: %+v", d)
	}
}

// A fix that ran without the person's yes (a table fix) is never recorded as approved.
func TestOnlyAPersonsYesMakesAProvenFix(t *testing.T) {
	ctx := context.Background()
	h, r, l1 := newProposalHarness(t)
	diag := Diagnosis{Kind: DiagEnvironment, FixCommands: []string{"echo ok"}, Source: "known:x", Learned: true, Signature: "E: x"}
	if !h.applyFix(ctx, r, diag, &BuildReport{}) {
		t.Fatal("a table fix did not run")
	}
	h.settleLearning(ctx, true)
	if got, _ := l1.ProvenFix(ctx, "E: x"); got != "" {
		t.Fatalf("a fix nobody approved was recorded as approved: %q", got)
	}
}
