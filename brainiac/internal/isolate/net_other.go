//go:build !linux

package isolate

import "os/exec"

// CanCutNetwork says this system has a way to start a process with no network.
const CanCutNetwork = false

// NoNetwork does nothing here: Windows and macOS have no unprivileged network
// namespace, and a caller reports the model process as having a network.
func NoNetwork(*exec.Cmd) {}
