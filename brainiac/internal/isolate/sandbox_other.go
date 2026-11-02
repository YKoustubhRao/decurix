//go:build !linux

package isolate

import (
	"errors"
	"os/exec"
	"path/filepath"
)

// CanSandbox says this kernel can confine a process's files. Landlock is Linux only.
func CanSandbox() bool { return false }

// CanBlockTCP says a sandbox can also refuse TCP connections.
func CanBlockTCP() bool { return false }

// Wrap cannot confine a process here; check CanSandbox first.
func Wrap(*exec.Cmd, Policy) error { return errors.New("no sandbox on this system") }

func resolved(program string) []string { return []string{filepath.Dir(program)} }
