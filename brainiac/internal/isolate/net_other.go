//go:build !linux

package isolate

import "os/exec"

// CanCutNetwork says this system has a way to start a process with no network.
const CanCutNetwork = false

// NoNetwork does nothing here: Windows and macOS have no unprivileged network
// namespace, and a caller reports the model process as having a network.
func NoNetwork(*exec.Cmd) {}

// DieWithParent does nothing here; Windows and macOS have no parent death signal.
func DieWithParent(*exec.Cmd) {}

// StartLocked starts cmd.
func StartLocked(cmd *exec.Cmd) error { return cmd.Start() }
