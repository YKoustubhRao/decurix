//go:build !windows

package desktop

import "os/exec"

// HideWindow does nothing off Windows: only there does a console program get
// a window of its own.
func HideWindow(*exec.Cmd) {}
