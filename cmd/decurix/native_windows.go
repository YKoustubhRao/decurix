//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// hideWindow keeps a console program started by the control plane - which has
// no console of its own when the app starts it - from opening a console
// window on the person's screen.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}
