//go:build windows

package hostshell

import (
	"os/exec"
	"strconv"
	"syscall"
)

// Name is what the shell is called, for messages and prompts.
const Name = "bash"

// argv runs line in bash on Windows too. TheBot runs on a Linux desktop; this
// file exists only so the module compiles (and its tests run) on a Windows
// development host, where Git's bash is on the PATH.
func argv(line string) (string, []string) {
	return "bash", []string{"-c", line}
}

func prepare(*exec.Cmd) {}

// OwnGroup starts cmd in a process group of its own. Call before Start.
func OwnGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// KillTree ends cmd and every process it started.
func KillTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run(); err != nil {
		_ = cmd.Process.Kill()
	}
}
