package desktop

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
)

// The exec half of fakeAPI: an exec runs the test's onExec with what the caller sent on stdin, and answers with
// what it printed and its exit status, framed the way Docker frames them.

// fakeExec is one exec the fake was asked to create.
type fakeExec struct {
	container string
	opts      container.ExecOptions
	done      chan struct{}
	code      int
}

// execRun is what a test's onExec decides for one exec: what it printed and how it exited.
type execRun func(containerName string, opts container.ExecOptions, stdin []byte) (stdout []byte, code int)

type fakeExecs struct {
	mu     sync.Mutex
	execs  map[string]*fakeExec
	seq    int
	onExec execRun
}

func (f *fakeAPI) ContainerExecCreate(_ context.Context, name string, opts container.ExecOptions) (container.ExecCreateResponse, error) {
	f.record("ContainerExecCreate %s %v user=%s", name, opts.Cmd, opts.User)
	f.ex.mu.Lock()
	defer f.ex.mu.Unlock()
	if f.ex.execs == nil {
		f.ex.execs = map[string]*fakeExec{}
	}
	f.ex.seq++
	id := fmt.Sprintf("exec-%d", f.ex.seq)
	f.ex.execs[id] = &fakeExec{container: name, opts: opts, done: make(chan struct{})}
	return container.ExecCreateResponse{ID: id}, nil
}

func (f *fakeAPI) ContainerExecAttach(_ context.Context, id string, _ container.ExecAttachOptions) (types.HijackedResponse, error) {
	f.ex.mu.Lock()
	e := f.ex.execs[id]
	run := f.ex.onExec
	f.ex.mu.Unlock()
	if e == nil {
		return types.HijackedResponse{}, fmt.Errorf("no exec %s", id)
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() {
		var stdin []byte
		if e.opts.AttachStdin {
			stdin, _ = io.ReadAll(inR)
		}
		var out []byte
		code := 0
		if run != nil {
			out, code = run(e.container, e.opts, stdin)
		}
		e.code = code
		close(e.done)
		w := stdcopy.NewStdWriter(outW, stdcopy.Stdout)
		_, _ = w.Write(out)
		_ = outW.Close()
	}()
	conn := &fakeConn{in: inW, out: outR}
	return types.HijackedResponse{Conn: conn, Reader: bufio.NewReader(outR)}, nil
}

func (f *fakeAPI) ContainerExecInspect(_ context.Context, id string) (container.ExecInspect, error) {
	f.ex.mu.Lock()
	e := f.ex.execs[id]
	f.ex.mu.Unlock()
	if e == nil {
		return container.ExecInspect{}, fmt.Errorf("no exec %s", id)
	}
	<-e.done
	return container.ExecInspect{ExecID: id, ExitCode: e.code}, nil
}

func (f *fakeAPI) ContainerStart(_ context.Context, name string, _ container.StartOptions) error {
	f.record("ContainerStart %s", name)
	return nil
}

func (f *fakeAPI) ContainerPause(_ context.Context, name string) error {
	f.record("ContainerPause %s", name)
	return nil
}

func (f *fakeAPI) ContainerUnpause(_ context.Context, name string) error {
	f.record("ContainerUnpause %s", name)
	return nil
}

// fakeConn is the hijacked connection of one exec: writes are its stdin, CloseWrite ends it.
type fakeConn struct {
	in  *io.PipeWriter
	out *io.PipeReader
}

func (c *fakeConn) Read(p []byte) (int, error)       { return c.out.Read(p) }
func (c *fakeConn) Write(p []byte) (int, error)      { return c.in.Write(p) }
func (c *fakeConn) CloseWrite() error                { return c.in.Close() }
func (c *fakeConn) Close() error                     { _ = c.in.Close(); return c.out.Close() }
func (c *fakeConn) LocalAddr() net.Addr              { return nil }
func (c *fakeConn) RemoteAddr() net.Addr             { return nil }
func (c *fakeConn) SetDeadline(time.Time) error      { return nil }
func (c *fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakeConn) SetWriteDeadline(time.Time) error { return nil }
