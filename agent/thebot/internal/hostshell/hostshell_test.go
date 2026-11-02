package hostshell

import (
	"context"
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
