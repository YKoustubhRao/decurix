package desktop

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
)

// TestADockerThatIsDownIsWaitedFor: with Docker not running, the state says so and the start-up that needs Docker
// waits; once Docker answers, the start-up runs exactly once, and a later outage only flips the state (W-08).
func TestADockerThatIsDownIsWaitedFor(t *testing.T) {
	var up atomic.Bool
	var readies atomic.Int32
	var state dockerState
	ping := func(context.Context) error {
		if up.Load() {
			return nil
		}
		return errors.New("connection refused")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchDocker(ctx, time.Millisecond, ping, &state, func(context.Context) error { readies.Add(1); return nil })

	waitFor(t, "", func() bool { return state.Load() == dockerDown })
	time.Sleep(20 * time.Millisecond)
	if readies.Load() != 0 {
		t.Fatal("the Docker start-up must not run while Docker is down")
	}
	up.Store(true)
	waitFor(t, "", func() bool { return state.Load() == dockerUp && readies.Load() == 1 })
	up.Store(false)
	waitFor(t, "", func() bool { return state.Load() == dockerDown })
	up.Store(true)
	waitFor(t, "", func() bool { return state.Load() == dockerUp })
	time.Sleep(20 * time.Millisecond)
	if n := readies.Load(); n != 1 {
		t.Fatalf("the start-up ran %d times, want once", n)
	}
}

// TestAFailedStartUpIsTriedAgain: a start-up that fails (the network could not be made) is run again on a later check.
func TestAFailedStartUpIsTriedAgain(t *testing.T) {
	var state dockerState
	var tries atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchDocker(ctx, time.Millisecond, func(context.Context) error { return nil }, &state, func(context.Context) error {
		if tries.Add(1) < 3 {
			return errors.New("not yet")
		}
		return nil
	})
	waitFor(t, "", func() bool { return tries.Load() == 3 })
	time.Sleep(20 * time.Millisecond)
	if tries.Load() != 3 {
		t.Fatalf("a start-up that succeeded must not run again: %d", tries.Load())
	}
}

// TestAManagerWithoutDockerSaysSo: no client is not reachable, and not reachable is not "not checked".
func TestAManagerWithoutDockerSaysSo(t *testing.T) {
	m := newManagerWith(nil)
	if m.DockerReachable() || m.DockerChecked() {
		t.Fatal("nothing checked yet")
	}
	ctx, cancel := context.WithCancel(context.Background())
	go m.WatchDocker(ctx, time.Millisecond, nil)
	waitFor(t, "", m.DockerChecked)
	cancel()
	if m.DockerReachable() {
		t.Fatal("a Manager with no client is not reachable")
	}
}

// TestGoneOnlyOnDockersOwnNo: a desktop is gone only when Docker says there is no such container; a timeout is no
// evidence, and a desktop being created is not gone.
func TestGoneOnlyOnDockersOwnNo(t *testing.T) {
	fake := &fakeAPI{inspect: func(name string) (container.InspectResponse, error) {
		switch name {
		case "desktop_here":
			return container.InspectResponse{}, nil
		case "desktop_slow":
			return container.InspectResponse{}, context.DeadlineExceeded
		}
		return container.InspectResponse{}, errFakeNotFound
	}}
	m := newManagerWith(fake)
	m.provisioning["new"] = &ProvisioningDesktop{}
	for id, want := range map[string]bool{"here": false, "slow": false, "away": true, "new": false} {
		if got, _ := m.Gone(context.Background(), id); got != want {
			t.Errorf("Gone(%s) = %v, want %v", id, got, want)
		}
	}
}
