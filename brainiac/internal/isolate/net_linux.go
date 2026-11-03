//go:build linux

package isolate

import (
	"os"
	"os/exec"
	"runtime"
	"syscall"
)

// CanCutNetwork says this system has a way to start a process with no network.
// The start can still be refused (a container, or an AppArmor rule against
// unprivileged namespaces), so a caller must handle cmd.Start failing.
const CanCutNetwork = true

// NoNetwork makes cmd start in its own network namespace, which has no
// interface up: no route out, and not even this machine's loopback. It runs
// as the same user, so it reads its files and uses its GPU as before.
// Wrapped afterwards (Wrap), it also gets a mount namespace holding only its
// policy's files. It dies with Brainiac.
func NoNetwork(cmd *exec.Cmd) {
	uid, gid := os.Getuid(), os.Getgid()
	attr := sysProcAttr(cmd)
	attr.Cloneflags |= syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET
	attr.UidMappings = []syscall.SysProcIDMap{{ContainerID: uid, HostID: uid, Size: 1}}
	attr.GidMappings = []syscall.SysProcIDMap{{ContainerID: gid, HostID: gid, Size: 1}}
}

// DieWithParent makes cmd's process get SIGKILL when Brainiac exits, however
// it exits, so no model server outlives it holding gigabytes. Pdeathsig
// fires when the starting thread ends, so a caller starts cmd from a locked
// thread (StartLocked).
func DieWithParent(cmd *exec.Cmd) { sysProcAttr(cmd).Pdeathsig = syscall.SIGKILL }

// sysProcAttr is cmd's SysProcAttr, made when it has none: the settings
// here are merged, never replaced.
func sysProcAttr(cmd *exec.Cmd) *syscall.SysProcAttr {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	return cmd.SysProcAttr
}

// StartLocked starts cmd from a thread locked for the call, so the parent
// death signal is tied to a thread that lives as long as Brainiac does.
func StartLocked(cmd *exec.Cmd) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	return cmd.Start()
}
