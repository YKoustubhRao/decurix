package desktop

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// xTCPListeners lists the X display ports (6000-6063) that /proc/net/tcp or tcp6 text shows listening.
func xTCPListeners(proc string) []int {
	var ports []int
	for _, line := range strings.Split(proc, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[3] != "0A" { // 0A: LISTEN
			continue
		}
		i := strings.LastIndex(fields[1], ":")
		if i < 0 {
			continue
		}
		port, err := strconv.ParseInt(fields[1][i+1:], 16, 32)
		if err == nil && port >= 6000 && port <= 6063 {
			ports = append(ports, int(port))
		}
	}
	return ports
}

// The desktop image tells Xvnc not to listen on TCP (W2 handoff: anything on the Docker network that reached X could
// drive the screen around the approval gate).
func TestTheDesktopImageKeepsXOffTCP(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docker", "desktop.basic.Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "echo 'nolisten=tcp' >> /etc/tigervnc/vncserver-config-mandatory") {
		t.Fatal("the desktop image must start Xvnc with -nolisten tcp")
	}
}

func TestXTCPListenersReadsProcNet(t *testing.T) {
	proc := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:170D 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1001        0 1 1 0 100 0 0 10 0
   1: 00000000:1771 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1001        0 2 1 0 100 0 0 10 0
   2: 0100007F:1771 0100007F:9C40 01 00000000:00000000 00:00000000 00000000  1001        0 3 1 0 100 0 0 10 0
   0: 00000000000000000000000000000000:1770 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1001 0 4 1
`
	got := xTCPListeners(proc)
	if len(got) != 2 || got[0] != 6001 || got[1] != 6000 {
		t.Fatalf("got %v, want the two listening X ports (6001, 6000), not VNC's 5901 or an open connection", got)
	}
}

// TestDesktopXServerHasNoTCPLive reads a running desktop's sockets: no X display port may listen on TCP. It never
// starts or builds anything; the owner runs it against a desktop recreated from a rebuilt image:
//
//	DECURIX_X11_LIVE_DESKTOP=desktop_<id> go test ./internal/desktop -run XServerHasNoTCPLive -v
func TestDesktopXServerHasNoTCPLive(t *testing.T) {
	name := os.Getenv("DECURIX_X11_LIVE_DESKTOP")
	if name == "" {
		t.Skip("set DECURIX_X11_LIVE_DESKTOP to a running desktop container")
	}
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	exec, err := cli.ContainerExecCreate(ctx, name, container.ExecOptions{Cmd: []string{"cat", "/proc/net/tcp", "/proc/net/tcp6"}, AttachStdout: true})
	if err != nil {
		t.Fatal(err)
	}
	attach, err := cli.ContainerExecAttach(ctx, exec.ID, container.ExecAttachOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer attach.Close()
	var out bytes.Buffer
	_, _ = stdcopy.StdCopy(&out, &bytes.Buffer{}, attach.Reader)
	if !strings.Contains(out.String(), "local_address") {
		t.Fatalf("could not read the desktop's sockets: %q", out.String())
	}
	if ports := xTCPListeners(out.String()); len(ports) > 0 {
		t.Fatalf("the desktop's X server listens on TCP %v: start Xvnc with -nolisten tcp and recreate the desktop", ports)
	}
}
