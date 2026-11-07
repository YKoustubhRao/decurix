package desktop

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/volume"
)

// pausedSlowTools is slowTools whose desktop "box" is paused: Start resumes it rather than starting it.
type pausedSlowTools struct {
	*slowTools
	resumed chan struct{}
}

func (d *pausedSlowTools) ContainerInspect(_ context.Context, name string) (container.InspectResponse, error) {
	return container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{ID: "id-" + name,
		State: &container.State{Paused: true, Status: container.StatePaused}}}, nil
}

func (d *pausedSlowTools) ContainerUnpause(context.Context, string) error {
	close(d.resumed)
	return nil
}

// slowPopulate is a manager whose tools populate holds /opt/tools until release is closed, already copying.
func slowPopulate(t *testing.T, state container.ContainerState) (*Manager, *slowTools) {
	t.Helper()
	fake := &fakeAPI{}
	fake.volumeGet = func(name string) (volume.Volume, error) { return volume.Volume{Name: name}, nil }
	d := &slowTools{toolsDaemon: &toolsDaemon{fakeAPI: fake}, release: make(chan struct{}), populated: make(chan struct{}),
		state: state}
	m := newManagerWith(d)
	go func() { _ = m.EnsureToolsVolume(context.Background()) }()
	<-d.populated
	t.Cleanup(func() {
		select {
		case <-d.release:
		default:
			close(d.release)
		}
	})
	return m, d
}

// A start that waits for a tools refresh waits a bounded time (RW1-G-04): past toolsStartWait it is refused, saying
// so, and the desktop is not started later behind the person's back.
func TestAStartGivesUpOnAToolsRefreshThatLasts(t *testing.T) {
	old := toolsStartWait
	toolsStartWait = 150 * time.Millisecond
	t.Cleanup(func() { toolsStartWait = old })
	m, d := slowPopulate(t, container.StateExited)

	began := time.Now()
	err := m.Start(context.Background(), "box")
	if !errors.Is(err, ErrToolsRefreshing) {
		t.Fatalf("a start past the wait gave %v, want ErrToolsRefreshing", err)
	}
	if waited := time.Since(began); waited > 2*time.Second {
		t.Fatalf("the start waited %s: the wait is not bounded", waited)
	}
	close(d.release)
	time.Sleep(100 * time.Millisecond)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.state == container.StateRunning {
		t.Fatal("the refused start started the desktop once the refresh ended")
	}
}

// A start whose request ends while it waits for a tools refresh returns then, and never starts the desktop later.
func TestAStartWaitingForAToolsRefreshEndsWithItsRequest(t *testing.T) {
	m, d := slowPopulate(t, container.StateExited)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := m.Start(ctx, "box"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a start whose request ended gave %v", err)
	}
	close(d.release)
	time.Sleep(100 * time.Millisecond)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.state == container.StateRunning {
		t.Fatal("the desktop was started after its request had ended")
	}
}

// Resuming a paused desktop through Start waits for a tools refresh in progress, as a start does: it never comes back
// while /opt/tools is being replaced (RW1-G-04).
func TestStartResumingAPausedDesktopWaitsForAToolsRefresh(t *testing.T) {
	fake := &fakeAPI{}
	fake.volumeGet = func(name string) (volume.Volume, error) { return volume.Volume{Name: name}, nil }
	base := &slowTools{toolsDaemon: &toolsDaemon{fakeAPI: fake}, release: make(chan struct{}),
		populated: make(chan struct{}), state: container.StatePaused}
	d := &pausedSlowTools{slowTools: base, resumed: make(chan struct{})}
	m := newManagerWith(d)
	go func() { _ = m.EnsureToolsVolume(context.Background()) }()
	<-base.populated
	defer func() {
		select {
		case <-base.release:
		default:
			close(base.release)
		}
	}()

	done := make(chan error, 1)
	go func() { done <- m.Start(context.Background(), "box") }()
	select {
	case <-d.resumed:
		t.Fatal("a paused desktop was resumed while the tools volume was being replaced")
	case <-time.After(300 * time.Millisecond):
	}
	close(base.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the resume never went ahead after the refresh")
	}
	select {
	case <-d.resumed:
	default:
		t.Fatal("Start did not resume the paused desktop")
	}
}
