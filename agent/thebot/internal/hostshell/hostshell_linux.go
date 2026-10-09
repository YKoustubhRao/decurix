package hostshell

import (
	"os/exec"
	"syscall"
)

// Name is what the shell is called, for messages and prompts.
const Name = "bash"

func argv(line string) (string, []string) {
	return "bash", []string{"-c", line}
}

func prepare(*exec.Cmd) {}

// OwnGroup makes cmd the leader of a process group of its own, so KillTree
// can end everything it started. Call before Start.
func OwnGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// KillTree ends cmd and every process it started.
func KillTree(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
