//go:build linux

package web

import (
	"context"
	"syscall"
	"testing"
)

// On Linux the headless browser is killed when Brainiac exits, however it
// exits, so no browser outlives it. Built, never started.
func TestTheBrowserDiesWithBrainiac(t *testing.T) {
	cmd := browserCommand(context.Background(), "browser", "127.0.0.1:1", t.TempDir(), "http://example.com/")
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Pdeathsig != syscall.SIGKILL {
		t.Fatalf("no parent death signal: %+v", cmd.SysProcAttr)
	}
}
