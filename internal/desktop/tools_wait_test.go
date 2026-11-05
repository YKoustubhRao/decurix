package desktop

import (
	"context"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/volume"
)

// toolsDaemon is a fake daemon whose tools populate container exits with exit.
type toolsDaemon struct {
	*fakeAPI
	exit     int64
	desktops []container.Summary
	imageID  string // the tools image's ID; "" is "sha256:tools-1"
}

func (d *toolsDaemon) ContainerList(_ context.Context, o container.ListOptions) ([]container.Summary, error) {
	if !o.All {
		return nil, nil // a list of running containers only would miss a paused desktop
	}
	return d.desktops, nil
}

func (d *toolsDaemon) ImageInspectWithRaw(context.Context, string) (image.InspectResponse, []byte, error) {
	id := d.imageID
	if id == "" {
		id = "sha256:tools-1"
	}
	return image.InspectResponse{ID: id}, nil, nil
}

func (d *toolsDaemon) ContainerWait(context.Context, string, container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	status := make(chan container.WaitResponse, 1)
	status <- container.WaitResponse{StatusCode: d.exit}
	return status, make(chan error)
}

// A tools refresh that waits for running desktops is told to the status banner, and tried again on the next
// desktop's creation until it applies (REVIEW-3 follow-up 2).
func TestAWaitingToolsRefreshIsShownAndTriedAgain(t *testing.T) {
	fake := &fakeAPI{}
	fake.volumeGet = func(name string) (volume.Volume, error) { return volume.Volume{Name: name}, nil }
	daemon := &toolsDaemon{fakeAPI: fake, exit: toolsWaitExit}
	m := newManagerWith(daemon)

	if err := m.EnsureToolsVolume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !m.ToolsRefreshWaiting(context.Background()) {
		t.Fatal("a refresh that waited must be shown as waiting")
	}
	daemon.exit = 0
	if err := m.EnsureToolsVolume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.ToolsRefreshWaiting(context.Background()) {
		t.Fatal("once applied, the refresh is no longer waiting")
	}
	if n := len(fake.called("ContainerCreate")); n != 2 {
		t.Fatalf("the waiting refresh must be tried again: %d populate runs", n)
	}
	if err := m.EnsureToolsVolume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.called("ContainerCreate")); n != 2 {
		t.Fatalf("an applied refresh is not run again: %d populate runs", n)
	}
}

// The populate is told desktops are running when one is paused or restarting, not only running: an idle-paused
// desktop resumed mid-task must find the tools it started with (RW1-G-02). Stopped ones do not hold a refresh back.
func TestAPausedDesktopMakesAToolsRefreshWait(t *testing.T) {
	for state, want := range map[container.ContainerState]string{
		container.StatePaused: "1", container.StateRestarting: "1", container.StateRunning: "1",
		container.StateExited: "0", container.StateCreated: "0",
	} {
		fake := &fakeAPI{}
		var env []string
		fake.create = func(_ string, cfg *container.Config, _ *container.HostConfig) (container.CreateResponse, error) {
			env = cfg.Env
			return container.CreateResponse{ID: "tools-init"}, nil
		}
		daemon := &toolsDaemon{fakeAPI: fake, desktops: []container.Summary{{Names: []string{"/desktop_box"}, State: state}}}
		m := newManagerWith(daemon)
		if err := m.PopulateToolsVolume(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(env) != 1 || env[0] != "DECURIX_DESKTOPS_RUNNING="+want {
			t.Errorf("a %s desktop: the populate was told %v, want running=%s", state, env, want)
		}
	}
}

// A tools image built after the volume was filled (the app builds it after the dashboard is up) is applied by the
// next creation with no desktop running, and shown as waiting until then (F-T: a latch kept the old tree for good).
func TestANewerToolsImageIsAppliedLater(t *testing.T) {
	fake := &fakeAPI{}
	fake.volumeGet = func(name string) (volume.Volume, error) { return volume.Volume{Name: name}, nil }
	daemon := &toolsDaemon{fakeAPI: fake}
	m := newManagerWith(daemon)

	if err := m.EnsureToolsVolume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.EnsureToolsVolume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.called("ContainerCreate")); n != 1 {
		t.Fatalf("an unchanged image is filled once: %d populate runs", n)
	}
	if m.ToolsRefreshWaiting(context.Background()) {
		t.Fatal("nothing waits while the volume holds the current image")
	}

	daemon.imageID = "sha256:tools-2"
	if !m.ToolsRefreshWaiting(context.Background()) {
		t.Fatal("a newer tools image not yet applied must be shown as waiting")
	}
	if err := m.EnsureToolsVolume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.called("ContainerCreate")); n != 2 {
		t.Fatalf("the newer image was not applied: %d populate runs", n)
	}
	if m.ToolsRefreshWaiting(context.Background()) {
		t.Fatal("an applied image is no longer waiting")
	}
}
