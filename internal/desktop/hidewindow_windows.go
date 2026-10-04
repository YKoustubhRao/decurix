//go:build windows

package desktop

import (
	"os/exec"
	"syscall"
)

// HideWindow keeps a console program the control plane starts from opening a
// console window on the person's screen. The Windows app starts the control
// plane with no console of its own, so every console child (PowerShell, the
// host helper) would otherwise get a new, visible one.
func HideWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
}
