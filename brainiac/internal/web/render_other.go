//go:build !linux

package web

import (
	"os/exec"

	"brainiac/internal/isolate"
)

// confineBrowser does nothing here: Landlock is Linux only. Windows and
// macOS keep the minimal environment, the guard proxy and the bounded read.
func confineBrowser(*exec.Cmd, isolate.Policy) error { return nil }
