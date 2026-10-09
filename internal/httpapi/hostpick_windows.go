//go:build windows

package httpapi

import (
	"os/exec"
	"syscall"
)

// hidePickerConsole starts the chooser's PowerShell with no console window.
// Not SysProcAttr.HideWindow: that makes the process's first window open
// hidden, and here the first window is what puts the chooser in front.
func hidePickerConsole(cmd *exec.Cmd) {
	const createNoWindow = 0x08000000

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}
