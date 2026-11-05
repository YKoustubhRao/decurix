package hostshell

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// A command whose child still holds the output pipe used to keep Wait
// blocked past its context: the shell was killed, its child was not.
func TestCommandContextReturnsWhenItsContextEnds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	cmd := CommandContext(ctx, "sleep 30; echo late")
	started := time.Now()
	out, err := cmd.Output()
	if err == nil {
		t.Fatalf("a killed command reported success, printed %q", out)
	}
	if waited := time.Since(started); waited > 10*time.Second {
		t.Fatalf("waited %s after the context ended", waited)
	}
}

// A command that exits 0 while a child it backgrounded still holds stdout
// ends in exec.ErrWaitDelay once WaitDelay cuts the pipe: that is the
// command's success, and Exited says so. A nonzero exit stays an error.
func TestExitedTakesACleanExitWithAHeldPipeAsSuccess(t *testing.T) {
	cmd := CommandContext(context.Background(), "sleep 6 & echo started")
	var out strings.Builder
	cmd.Stdout = &out
	err := cmd.Run()
	if err == nil {
		t.Skip("this shell did not leave the pipe held by the background child")
	}
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("Run() = %v, want exec.ErrWaitDelay", err)
	}
	if got := Exited(cmd, err); got != nil {
		t.Fatalf("Exited(ErrWaitDelay after exit 0) = %v, want nil", got)
	}
	if !strings.Contains(out.String(), "started") {
		t.Errorf("stdout = %q", out.String())
	}

	bad := CommandContext(context.Background(), "exit 3")
	if Exited(bad, bad.Run()) == nil {
		t.Fatal("Exited took exit 3 as success")
	}
}
