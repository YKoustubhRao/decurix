package desktop

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// The command a practice step runs is run here for real, as the desktop runs it: workerCmd(timedCommand(...)) through
// sh -c, with the system's env and coreutils timeout. A string check let "timeout -k 5 3s -- cmd" through, which
// coreutils runs as the command "--" (exit 127, every oracle step of a live L2). Linux only, where those exist.
func TestATimedPracticeStepRunsAndIsEndedAtItsTime(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs Linux's /usr/bin/env and coreutils timeout")
	}
	for _, tool := range []string{"/usr/bin/env", "/usr/bin/timeout", "/bin/sh"} {
		if _, err := os.Stat(tool); err != nil {
			t.Skipf("no %s here", tool)
		}
	}
	run := func(timeout time.Duration, argv ...string) (int, time.Duration) {
		t.Helper()
		line := workerCmd(timedCommand(timeout, argv)...)
		started := time.Now()
		err := exec.Command("/bin/sh", append([]string{"-c", `exec "$@"`, "sh"}, line...)...).Run()
		var exit *exec.ExitError
		switch {
		case err == nil:
			return 0, time.Since(started)
		case errors.As(err, &exit):
			return exit.ExitCode(), time.Since(started)
		default:
			t.Fatalf("%v: %v", line, err)
			return -1, 0
		}
	}
	if code, _ := run(5*time.Second, "sh", "-c", "exit 3"); code != 3 {
		t.Fatalf("a step's own exit code: got %d, want 3 (127 is timeout running the wrong command)", code)
	}
	if code, _ := run(5*time.Second, "true"); code != 0 {
		t.Fatalf("true under timeout exited %d", code)
	}
	// Ended at its time: timeout's 124, well before the step would have finished (the bound only turns a hang into a
	// failure).
	if code, took := run(time.Second, "sleep", "60"); code != 124 || took > 30*time.Second {
		t.Fatalf("a step past its time: exit %d after %v, want 124 soon after 1 s", code, took)
	}
}
