package skills

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// shellResult is what one command did.
type shellResult struct {
	ExitCode int
	Output   string
	TimedOut bool
}

// maxShellCapture bounds how much output one command may hold in memory.
// The model only ever sees a truncated view of it (see formatCommandResult);
// this is the ceiling on what `cat` of a large file can cost the process.
const maxShellCapture = 256 * 1024

// cappedBuffer keeps the first maxShellCapture bytes and counts the rest.
type cappedBuffer struct {
	buf     bytes.Buffer
	dropped int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	room := maxShellCapture - c.buf.Len()
	if room <= 0 {
		c.dropped += len(p)
		return len(p), nil
	}
	if len(p) > room {
		c.buf.Write(p[:room])
		c.dropped += len(p) - room
		return len(p), nil
	}
	return c.buf.Write(p)
}

// runShell runs one command non-interactively, with stdout and stderr
// interleaved the way a person at a terminal would see them.
//
// Three details each fix a way a real agent command hung or lied:
//   - stdin is empty, so anything that prompts ("Do you want to continue?
//     [Y/n]") reads EOF and fails fast instead of waiting forever.
//   - the command gets its own process group, and a timeout kills the whole
//     group - `npm start` killed alone leaves its node child running.
//   - WaitDelay stops Wait blocking on a pipe a backgrounded child still
//     holds open: `python3 -m http.server &` returns at once in a terminal,
//     and would otherwise hang here until the timeout.
func runShell(ctx context.Context, dir, command string, timeout time.Duration, extraEnv ...string) shellResult {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.Command("bash", "-c", command)
	cmd.Dir = dir
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	cmd.Stdin = nil
	var out cappedBuffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second

	if err := cmd.Start(); err != nil {
		return shellResult{ExitCode: -1, Output: "could not start bash: " + err.Error()}
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var err error
	timedOut := false
	select {
	case err = <-done:
	case <-runCtx.Done():
		timedOut = errors.Is(runCtx.Err(), context.DeadlineExceeded)
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		err = <-done
	}

	res := shellResult{Output: out.buf.String(), TimedOut: timedOut}
	if out.dropped > 0 {
		res.Output += "\n... [output continued; " + strconv.Itoa(out.dropped) + " more bytes not captured]"
	}

	var exitErr *exec.ExitError
	switch {
	case err == nil:
		res.ExitCode = 0
	case errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil:
		// The command itself finished; a background child it started is
		// what still held the pipe. Its own exit code is the real result.
		res.ExitCode = cmd.ProcessState.ExitCode()
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		res.ExitCode = -1
		if res.Output == "" {
			res.Output = err.Error()
		}
	}
	if timedOut {
		res.ExitCode = 124
		res.Output += "\n[killed: still running after " + timeout.String() + "]"
	}
	return res
}
