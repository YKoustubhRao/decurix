// Tests that a failed fill of the shared tools volume is told (ToolsNotReady, by class, never the error's text):
// the tools image empties the volume before it copies, so desktops may find no tools until a fill succeeds.
package desktop

import (
	"context"
	"errors"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/volume"
)

func TestAFailedToolsFillIsToldUntilOneSucceeds(t *testing.T) {
	fake := &fakeAPI{}
	fake.volumeGet = func(name string) (volume.Volume, error) { return volume.Volume{Name: name}, nil }
	daemon := &toolsDaemon{fakeAPI: fake, exit: 1}
	m := newManagerWith(daemon)

	if m.ToolsNotReady() != "" {
		t.Fatal("before any fill, nothing has failed")
	}
	if err := m.EnsureToolsVolume(context.Background()); err == nil {
		t.Fatal("a copy that exited 1 must fail")
	}
	if got := m.ToolsNotReady(); got != ToolsNotReadyCopy {
		t.Fatalf("after a failed copy: %q, want %q", got, ToolsNotReadyCopy)
	}
	// A fill that waits for running desktops left the volume as it was: it does not clear a failure.
	daemon.exit = toolsWaitExit
	if err := m.EnsureToolsVolume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := m.ToolsNotReady(); got != ToolsNotReadyCopy {
		t.Fatalf("a waiting fill cleared the failure: %q", got)
	}
	daemon.exit = 0
	if err := m.EnsureToolsVolume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := m.ToolsNotReady(); got != "" {
		t.Fatalf("after a fill that succeeded: %q", got)
	}
}

func TestAToolsFillThatCannotStartIsToldAsStart(t *testing.T) {
	fake := &fakeAPI{}
	fake.volumeGet = func(name string) (volume.Volume, error) { return volume.Volume{Name: name}, nil }
	fake.create = func(string, *container.Config, *container.HostConfig) (container.CreateResponse, error) {
		return container.CreateResponse{}, errors.New("no such image: secret detail")
	}
	m := newManagerWith(&toolsDaemon{fakeAPI: fake})
	if err := m.PopulateToolsVolume(context.Background()); err == nil {
		t.Fatal("a copy whose container cannot be made must fail")
	}
	if got := m.ToolsNotReady(); got != ToolsNotReadyStart {
		t.Fatalf("got %q, want %q", got, ToolsNotReadyStart)
	}
}
