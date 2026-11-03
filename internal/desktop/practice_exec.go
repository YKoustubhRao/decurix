package desktop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
)

// practiceExecOutputLimit bounds what ExecPracticeAsWorker hands back: stdout and stderr together.
const practiceExecOutputLimit = 1 << 20

// ErrNotPractice is ExecPracticeAsWorker's refusal of a container that is not a practice desktop.
var ErrNotPractice = errors.New("desktop: not a practice desktop")

// ExecPracticeAsWorker runs argv in a practice desktop as the desktop's user, with the worker's empty environment
// (workerCmd), and returns its exit status and its output (stdout and stderr interleaved as written, at most 1 MB;
// past that the output ends with a note saying how much was not kept). Only a container labelled as a practice
// desktop is accepted: a worker's own desktop, a mock app or anything else is refused before anything runs. The
// eval's oracle arm runs a case's scripted solution through it.
func (m *Manager) ExecPracticeAsWorker(ctx context.Context, containerID string, argv []string, timeout time.Duration) (int, []byte, error) {
	if len(argv) == 0 {
		return 0, nil, errors.New("desktop: nothing to run")
	}
	inspect, err := m.client.ContainerInspect(ctx, containerID)
	if err != nil {
		return 0, nil, err
	}
	if inspect.Config == nil || inspect.Config.Labels[PracticeLabel] != "true" || inspect.Config.Labels["decurix-practice-mock"] == "true" {
		return 0, nil, ErrNotPractice
	}
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	// The command is killed in the desktop when its time is up (coreutils timeout: TERM, then KILL 5 s later), not
	// only left behind: a step that ran on would change the mock's state after it was graded (review CP-B N6). The
	// wait here allows for that kill.
	ctx, cancel := context.WithTimeout(ctx, timeout+10*time.Second)
	defer cancel()

	// Whole seconds, rounded up: a timeout under a second is 1s, never 0s (which coreutils reads as no limit).
	secs := int((timeout + time.Second - 1) / time.Second)
	cmd := append([]string{"/usr/bin/timeout", "-k", "5", fmt.Sprintf("%ds", secs), "--"}, argv...)
	exec, err := m.client.ContainerExecCreate(ctx, inspect.ID, container.ExecOptions{
		User: "headless", Cmd: workerCmd(cmd...), AttachStdout: true, AttachStderr: true,
	})
	if err != nil {
		return 0, nil, err
	}
	attach, err := m.client.ContainerExecAttach(ctx, exec.ID, container.ExecAttachOptions{})
	if err != nil {
		return 0, nil, err
	}
	defer attach.Close()

	out := &cappedOutput{limit: practiceExecOutputLimit}
	done := make(chan error, 1)
	go func() {
		_, err := stdcopy.StdCopy(out, out, attach.Reader)
		done <- err
	}()
	var copyErr error
	select {
	case <-ctx.Done():
		attach.Close()
		<-done // the copy stops once the connection is closed; only then is out read
		return 0, out.result(), fmt.Errorf("desktop: the command did not finish in %s", timeout)
	case copyErr = <-done:
	}
	if copyErr != nil && !errors.Is(copyErr, io.EOF) {
		return 0, out.result(), copyErr
	}
	state, err := m.client.ContainerExecInspect(context.WithoutCancel(ctx), exec.ID)
	if err != nil {
		return 0, out.result(), err
	}
	if state.Running {
		return 0, out.result(), errors.New("desktop: the command did not finish")
	}
	if state.ExitCode == 124 || state.ExitCode == 137 {
		return state.ExitCode, out.result(), fmt.Errorf("desktop: the command did not finish in %s, and was ended", timeout)
	}
	return state.ExitCode, out.result(), nil
}

// cappedOutput keeps the first limit bytes written to it and counts the rest.
type cappedOutput struct {
	bytes.Buffer
	limit   int
	dropped int64
}

func (c *cappedOutput) Write(p []byte) (int, error) {
	room := max(min(c.limit-c.Len(), len(p)), 0)
	c.Buffer.Write(p[:room])
	c.dropped += int64(len(p) - room)
	return len(p), nil
}

// result is what was kept, with a note at the end when anything was dropped.
func (c *cappedOutput) result() []byte {
	out := append([]byte(nil), c.Bytes()...)
	if c.dropped > 0 {
		out = append(out, fmt.Sprintf("\n[%d more bytes of output not kept]\n", c.dropped)...)
	}
	return out
}
