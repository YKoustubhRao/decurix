package desktop

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// TestImageHasNoSetuidFilesLive lists the setuid and setgid files of a built desktop image (B-21): there must be none.
// It never builds or pulls; the owner runs it after a rebuild:
//
//	DECURIX_SETUID_LIVE_IMAGE=decurix-desktop:basic go test ./internal/desktop -run SetuidFilesLive -v
func TestImageHasNoSetuidFilesLive(t *testing.T) {
	image := os.Getenv("DECURIX_SETUID_LIVE_IMAGE")
	if image == "" {
		t.Skip("set DECURIX_SETUID_LIVE_IMAGE to an existing local desktop image")
	}
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	created, err := cli.ContainerCreate(ctx, &container.Config{Image: image, User: "0:0",
		Entrypoint: []string{"/usr/bin/find", "/", "-xdev", "(", "-perm", "-4000", "-o", "-perm", "-2000", ")", "-type", "f"}},
		&container.HostConfig{NetworkMode: "none"}, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cli.ContainerRemove(context.Background(), created.ID, container.RemoveOptions{Force: true}) }()
	if err := cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		t.Fatal(err)
	}
	waitC, errC := cli.ContainerWait(ctx, created.ID, container.WaitConditionNotRunning)
	select {
	case <-waitC:
	case err := <-errC:
		t.Fatal(err)
	}
	logs, err := cli.ContainerLogs(ctx, created.ID, container.LogsOptions{ShowStdout: true})
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	var out bytes.Buffer
	_, _ = stdcopy.StdCopy(&out, &bytes.Buffer{}, logs)
	if s := strings.TrimSpace(out.String()); s != "" {
		t.Fatalf("the image still has setuid or setgid files:\n%s", s)
	}
}
