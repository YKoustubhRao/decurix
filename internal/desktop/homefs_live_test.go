package desktop

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
)

// TestHomeCopiesCannotReachPastTheWorkerLive proves, against a real daemon, what homefs.go is for (B-01): a folder in
// the home that is a link to a root-only folder neither gives up that folder's files nor takes new ones, for a running
// and for a stopped desktop. It never builds or pulls images; the owner runs it with an existing desktop image:
//
//	DECURIX_HOMEFS_LIVE_IMAGE=<desktop image> go test ./internal/desktop -run Live -v
func TestHomeCopiesCannotReachPastTheWorkerLive(t *testing.T) {
	image := os.Getenv("DECURIX_HOMEFS_LIVE_IMAGE")
	if image == "" {
		t.Skip("set DECURIX_HOMEFS_LIVE_IMAGE to an existing local desktop image")
	}
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	m := NewManager(cli)
	const id = "homefslive"
	name, vol := "desktop_"+id, "desktop_"+id+"_home"
	_ = cli.ContainerRemove(ctx, name, container.RemoveOptions{Force: true})
	_ = cli.VolumeRemove(ctx, vol, true)
	if _, err := cli.VolumeCreate(ctx, volume.CreateOptions{Name: vol}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cli.VolumeRemove(context.Background(), vol, true) }()
	// As root: a root-only folder outside the home, and a link to it inside the home owned by the worker.
	setup := `mkdir -m 700 /rootonly && echo kept-out > /rootonly/f && chown 1001:1001 /home/headless && ` +
		`ln -s /rootonly /home/headless/link && chown -h 1001:1001 /home/headless/link && exec sleep 600`
	created, err := cli.ContainerCreate(ctx, &container.Config{Image: image, User: "0:0",
		Entrypoint: []string{"/bin/sh", "-c", setup}},
		&container.HostConfig{NetworkMode: "none", Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: vol, Target: homeDir}}},
		nil, nil, name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cli.ContainerRemove(context.Background(), name, container.RemoveOptions{Force: true}) }()
	if err := cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)

	check := func(state string) {
		if rc, err := m.CopyOut(ctx, id, homeDir+"/link/f"); err == nil {
			b, _ := io.ReadAll(rc)
			rc.Close()
			if strings.Contains(string(b), "kept-out") {
				t.Errorf("%s: a root-only file was read through the worker's link", state)
			}
		}
		_ = m.CopyFiles(ctx, id, homeDir+"/link", []File{{Name: "planted", Data: []byte("x")}})
	}
	check("running")
	if err := cli.ContainerStop(ctx, created.ID, container.StopOptions{}); err != nil {
		t.Fatal(err)
	}
	check("stopped")

	// Look at the root-only folder as root (the test's own root-side read of a fixed path, on the stopped container).
	if rc, _, err := cli.CopyFromContainer(ctx, created.ID, "/rootonly/planted"); err == nil {
		rc.Close()
		t.Error("a file was written into a root-only folder through the worker's link")
	}
}
