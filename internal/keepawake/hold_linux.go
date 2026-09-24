package keepawake

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

func newHolder() (holder, string) {
	if inContainer() {
		return nil, containerNote
	}

	// Under WSL it is Windows that sleeps, taking the Linux VM with it.
	if isWSL() {
		ps := windowsPowerShell()
		if ps == "" {
			return nil, "running under WSL, but powershell.exe was not found, so Windows may still sleep"
		}

		return &pipeHolder{command: func() *exec.Cmd {
			return exec.Command(ps, "-NoProfile", "-NonInteractive", "-EncodedCommand", encodedCommand(powerShellHelper))
		}}, "Windows, through powershell.exe from WSL"
	}

	inhibit, err := exec.LookPath("systemd-inhibit")
	if err != nil {
		return nil, "systemd-inhibit is not available, so this machine may still sleep"
	}

	pid := strconv.Itoa(os.Getpid())

	return &processHolder{command: func() *exec.Cmd {
		// tail --pid ends with this process, so the lock cannot outlive it
		// even if it is killed outright.
		cmd := exec.Command(inhibit, "--what=idle:sleep", "--who=Decurix", "--why=Watching mailboxes and running schedules",
			"--mode=block", "tail", "--pid="+pid, "-f", "/dev/null")
		cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}

		return cmd
	}}, "a systemd-inhibit lock"
}

func isWSL() bool {
	if os.Getenv("WSL_DISTRO_NAME") != "" {
		return true
	}

	release, err := os.ReadFile("/proc/sys/kernel/osrelease")

	return err == nil && strings.Contains(strings.ToLower(string(release)), "microsoft")
}

func windowsPowerShell() string {
	if p, err := exec.LookPath("powershell.exe"); err == nil {
		return p
	}

	for _, p := range []string{
		"/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe",
		"/mnt/c/WINDOWS/System32/WindowsPowerShell/v1.0/powershell.exe",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}

	return ""
}
