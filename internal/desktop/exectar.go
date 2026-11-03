package desktop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"path"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
)

// Offline containers (readiness programs, Simulate's mock app) run with a
// read-only root and their working folder on tmpfs. Docker refuses
// CopyToContainer into such a container, and CopyFromContainer does not see a
// tmpfs's contents, so files go in and out through tar run by exec instead.

// errExecTarMissing is execTarOut's answer when the path does not exist.
var errExecTarMissing = errors.New("path not found in the container")

// execTarMissingCode is the exit status execTarOut's script gives for a missing path.
const execTarMissingCode = 44

// execTarIn extracts a tar archive into dir inside a running container, as the
// unprivileged user, in archive order (so a file written last can signal readiness).
// flags go to tar before the archive.
func (m *Manager) execTarIn(ctx context.Context, id, dir string, archive io.Reader, flags ...string) error {
	cmd := append([]string{"/usr/bin/env", "-i", "PATH=/usr/bin:/bin", "tar", "-x", "-C", dir}, flags...)
	cmd = append(cmd, "-f", "-")
	exec, err := m.client.ContainerExecCreate(ctx, id, container.ExecOptions{User: "1001:1001", Cmd: cmd, AttachStdin: true, AttachStdout: true, AttachStderr: true})
	if err != nil {
		return err
	}
	attach, err := m.client.ContainerExecAttach(ctx, exec.ID, container.ExecAttachOptions{})
	if err != nil {
		return err
	}
	defer attach.Close()
	if _, err = io.Copy(attach.Conn, archive); err != nil {
		return err
	}
	if err = attach.CloseWrite(); err != nil {
		return err
	}
	var out bytes.Buffer
	if _, err = stdcopy.StdCopy(&out, &out, io.LimitReader(attach.Reader, 64<<10)); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	state, err := m.client.ContainerExecInspect(ctx, exec.ID)
	if err != nil {
		return err
	}
	if state.Running || state.ExitCode != 0 {
		log.Printf("desktop: tar into %s:%s exited %d: %s", shortID(id), dir, state.ExitCode, strings.TrimSpace(out.String()))
		return fmt.Errorf("copying files into the container failed")
	}
	return nil
}

// execTarOut reads one path of a running container as a tar, bounded by limit. Offline containers only (readiness,
// Simulate's mock): it freezes every process of the worker's uid and never resumes them, so on a live desktop it
// would stop the worker for good - a desktop's home is read with ReadTar (homefs.go) instead.
// Every other process the unprivileged user owns is stopped first in the same
// exec, so nothing can change the files while they are read. A paused container
// is resumed for the read and paused again after it.
func (m *Manager) execTarOut(ctx context.Context, id, from string, limit int64) ([]byte, error) {
	from = path.Clean(from)
	if !path.IsAbs(from) || from == "/" {
		return nil, fmt.Errorf("execTarOut needs an absolute path")
	}
	inspect, err := m.client.ContainerInspect(ctx, id)
	if err != nil {
		return nil, err
	}
	if inspect.State != nil && inspect.State.Paused {
		if err := m.client.ContainerUnpause(ctx, id); err != nil {
			return nil, err
		}
		defer func() { _ = m.client.ContainerPause(context.WithoutCancel(ctx), id) }()
	}
	script := fmt.Sprintf(`kill -s STOP -- -1 2>/dev/null; [ -e "$1" ] || exit %d; exec tar -c -C "$2" -- "$3"`, execTarMissingCode)
	cmd := []string{"/usr/bin/env", "-i", "PATH=/usr/bin:/bin", "/bin/sh", "-c", script, "sh", from, path.Dir(from), path.Base(from)}
	exec, err := m.client.ContainerExecCreate(ctx, id, container.ExecOptions{User: "1001:1001", Cmd: cmd, AttachStdout: true, AttachStderr: true})
	if err != nil {
		return nil, err
	}
	attach, err := m.client.ContainerExecAttach(ctx, exec.ID, container.ExecAttachOptions{})
	if err != nil {
		return nil, err
	}
	defer attach.Close()
	out := &cappedBuffer{limit: limit}
	if _, err = stdcopy.StdCopy(out, io.Discard, attach.Reader); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, errCapped) {
		return nil, err
	}
	if out.over {
		return nil, fmt.Errorf("the container's files exceed the copy limit")
	}
	state, err := m.client.ContainerExecInspect(ctx, exec.ID)
	if err != nil {
		return nil, err
	}
	switch {
	case state.Running:
		return nil, fmt.Errorf("reading the container's files did not finish")
	case state.ExitCode == execTarMissingCode:
		return nil, errExecTarMissing
	case state.ExitCode != 0:
		log.Printf("desktop: tar out of %s:%s exited %d", shortID(id), from, state.ExitCode)
		return nil, fmt.Errorf("reading the container's files failed")
	}
	return out.Bytes(), nil
}

// errCapped stops a copy that passed its limit.
var errCapped = errors.New("copy limit reached")

// cappedBuffer keeps at most limit bytes and notes when more arrived.
type cappedBuffer struct {
	bytes.Buffer
	limit int64
	over  bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if int64(b.Len()+len(p)) > b.limit {
		b.over = true
		return 0, errCapped
	}
	return b.Buffer.Write(p)
}

// headBuffer keeps the first limit bytes written to it and drops the rest, never failing a write: a sink for
// messages, whose overflow must not stop the stream it rides on.
type headBuffer struct {
	bytes.Buffer
	limit int
}

func (b *headBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.Len(); room > 0 {
		b.Buffer.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// shortID is a container id short enough for a log line.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
