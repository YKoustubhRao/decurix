//go:build linux

package isolate

import (
	"os"
	"os/exec"
	"syscall"
)

// CanCutNetwork says this system has a way to start a process with no network.
// The start can still be refused (a container, or an AppArmor rule against
// unprivileged namespaces), so a caller must handle cmd.Start failing.
const CanCutNetwork = true

// NoNetwork makes cmd start in its own network namespace, which has no
// interface up: no route out, and not even this machine's loopback. It runs
// as the same user, so it reads its files and uses its GPU as before.
func NoNetwork(cmd *exec.Cmd) {
	uid, gid := os.Getuid(), os.Getgid()
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: uid, HostID: uid, Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: gid, HostID: gid, Size: 1}},
	}
}
