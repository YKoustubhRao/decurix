//go:build !windows

package httpapi

import "os/exec"

// hidePickerConsole does nothing here: only Windows opens a console window.
func hidePickerConsole(*exec.Cmd) {}
