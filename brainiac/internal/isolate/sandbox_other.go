//go:build !linux

package isolate

import (
	"errors"
	"os/exec"
)

// CanSandbox says this kernel can confine a process's files. Landlock is Linux only.
func CanSandbox() bool { return false }

// CanBlockTCP says a sandbox can also refuse TCP connections.
func CanBlockTCP() bool { return false }

// Wrap cannot confine a process here; check CanSandbox first.
func Wrap(*exec.Cmd, Policy) error { return errors.New("no sandbox on this system") }

// resolved is the program itself; no sandbox applies here anyway.
func resolved(program string) []string { return []string{program} }

// CanConfine is false here: no network or mount namespace for a process.
func CanConfine() (bool, error) {
	return false, errors.New("only Linux can start a process with no network and only its own files")
}
