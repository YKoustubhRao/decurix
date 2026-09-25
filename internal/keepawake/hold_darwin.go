package keepawake

import (
	"os"
	"os/exec"
	"strconv"
)

func newHolder() (holder, string) {
	caffeinate, err := exec.LookPath("caffeinate")
	if err != nil {
		return nil, "caffeinate is not available, so this Mac may still sleep"
	}

	pid := strconv.Itoa(os.Getpid())

	// -i: no idle sleep; -s: no system sleep while on mains power; -w: end
	// with this process, however it ends.
	return &processHolder{command: func() *exec.Cmd {
		return exec.Command(caffeinate, "-i", "-s", "-w", pid)
	}}, "caffeinate"
}
