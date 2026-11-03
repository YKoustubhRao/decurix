package desktop

import (
	"context"
	"errors"
	"log"
	"sync/atomic"
	"time"

	"github.com/docker/docker/client"
)

// Docker reachability. The control plane used to exit when Docker did not answer at start (W-08): a first-time user
// whose Docker Desktop was not running yet got a cryptic _ping error and no dashboard. Now the listeners start anyway,
// the dashboard says Docker is not running, and the Docker-dependent start-up runs once Docker answers.

// errNoDocker is a Manager made without a Docker client.
var errNoDocker = errors.New("no Docker client")

// dockerState is what the last check of the Docker daemon found: 0 not checked yet, 1 reachable, 2 not reachable.
type dockerState = atomic.Int32

const (
	dockerUnknown int32 = iota
	dockerUp
	dockerDown
)

// DockerReachable reports whether the Docker daemon answered the Manager's last check (WatchDocker). It is cheap - no
// round trip - and false until the first check has answered, so a dashboard asks again rather than guess.
func (m *Manager) DockerReachable() bool { return m.docker.Load() == dockerUp }

// DockerChecked reports whether WatchDocker has checked the daemon at least once, so a page can tell "not running"
// from "not asked yet".
func (m *Manager) DockerChecked() bool { return m.docker.Load() != dockerUnknown }

// pingDocker asks the daemon whether it is there.
func (m *Manager) pingDocker(ctx context.Context) error {
	if m.client == nil {
		return errNoDocker
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := m.client.Ping(ctx)
	return err
}

// WatchDocker checks the Docker daemon every interval until ctx ends, keeping DockerReachable current. The first time
// the daemon answers it runs ready - the start-up that needs Docker - and, if ready fails, runs it again on a later
// answer until it succeeds once. ready runs on this goroutine, so it is never run twice at once.
func (m *Manager) WatchDocker(ctx context.Context, interval time.Duration, ready func(context.Context) error) {
	watchDocker(ctx, interval, m.pingDocker, &m.docker, ready)
}

// watchDocker is WatchDocker over any ping, for a test's stand-in daemon.
func watchDocker(ctx context.Context, interval time.Duration, ping func(context.Context) error, state *dockerState,
	ready func(context.Context) error) {
	done := ready == nil
	said := false
	for {
		if err := ping(ctx); err != nil {
			state.Store(dockerDown)
			if !said {
				log.Printf("Docker is not reachable (%v): the dashboard is up and says so; desktops start once Docker runs", err)
				said = true
			}
		} else {
			if state.Swap(dockerUp) == dockerDown {
				log.Printf("Docker is reachable again")
			}
			said = false
			if !done {
				if err := ready(ctx); err != nil {
					log.Printf("start-up that needs Docker: %v (trying again)", err)
				} else {
					done = true
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// Gone reports whether desktop id's container is gone: Docker says there is no such container and it is not being
// created. Any other answer - an error included - is not evidence that it is gone.
func (m *Manager) Gone(ctx context.Context, id string) (bool, error) {
	if m.client == nil {
		return false, errNoDocker
	}
	sanitizedID := sanitizeName(id)
	m.provisioningMu.RLock()
	_, creating := m.provisioning[sanitizedID]
	m.provisioningMu.RUnlock()
	if creating {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := m.client.ContainerInspect(ctx, "desktop_"+sanitizedID); err != nil {
		if client.IsErrNotFound(err) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}
