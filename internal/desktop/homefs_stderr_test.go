package desktop

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
)

// stderrFake answers the tar -c exec with: first entry on stdout, >4 KiB of warnings on stderr, second entry, exit 0.
type stderrFake struct{ *fakeAPI }

func (s stderrFake) ContainerExecAttach(ctx context.Context, id string, o container.ExecAttachOptions) (types.HijackedResponse, error) {
	s.ex.mu.Lock()
	e := s.ex.execs[id]
	s.ex.mu.Unlock()
	if !strings.Contains(strings.Join(e.opts.Cmd, " "), "tar -c") {
		return s.fakeAPI.ContainerExecAttach(ctx, id, o)
	}
	var a, b bytes.Buffer
	tw := tar.NewWriter(&a)
	_ = tw.WriteHeader(&tar.Header{Name: "d/one", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3})
	_, _ = tw.Write([]byte("one"))
	tw.Flush()
	full := a.Len()
	tw2 := tar.NewWriter(&b)
	_ = tw2.WriteHeader(&tar.Header{Name: "d/two", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3})
	_, _ = tw2.Write([]byte("two"))
	_ = tw2.Close()
	_ = full
	outR, outW := io.Pipe()
	e.code = 0
	close(e.done)
	go func() {
		_, _ = stdcopy.NewStdWriter(outW, stdcopy.Stdout).Write(a.Bytes())
		_, _ = stdcopy.NewStdWriter(outW, stdcopy.Stderr).Write(bytes.Repeat([]byte("tar: x: socket ignored\n"), 300))
		_, _ = stdcopy.NewStdWriter(outW, stdcopy.Stdout).Write(b.Bytes())
		_ = outW.Close()
	}()
	return types.HijackedResponse{Conn: &fakeConn{in: func() *io.PipeWriter { _, w := io.Pipe(); return w }(), out: outR}, Reader: bufio.NewReader(outR)}, nil
}

// Many tar warnings on stderr never cut the archive short with no error (review RW1-C-01): the whole archive comes
// through, and only the stdout limit may stop a copy.
func TestTarWarningsNeverCutTheArchiveShort(t *testing.T) {
	fake, _ := homeDesktop(true, false)
	m := newManagerWith(stderrFake{fake})
	rc, err := m.ReadTar(context.Background(), "box", homeDir+"/present", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	var names []string
	var rerr error
	for {
		h, err := tr.Next()
		if err != nil {
			if err != io.EOF {
				rerr = err
			}
			break
		}
		names = append(names, h.Name)
	}
	t.Logf("entries=%v err=%v", names, rerr)
	if rerr != nil || len(names) != 2 {
		t.Fatalf("archive cut short with no error: got %v", names)
	}
}
