package desktop

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
)

// pausingDaemon is homeDesktop's daemon with a pause state that Pause and Unpause change, and an Unpause that is
// slow enough for a second copy to read "paused" in the meantime. resumedFirst makes the first Unpause find the
// desktop already resumed by someone else (Docker answers "not paused").
type pausingDaemon struct {
	*fakeAPI
	mu           sync.Mutex
	paused       bool
	resumedFirst bool
}

func (p *pausingDaemon) ContainerInspect(ctx context.Context, name string) (container.InspectResponse, error) {
	r, err := p.fakeAPI.ContainerInspect(ctx, name)
	if err == nil {
		p.mu.Lock()
		r.State = &container.State{Running: true, Paused: p.paused}
		p.mu.Unlock()
	}
	return r, err
}

func (p *pausingDaemon) ContainerUnpause(ctx context.Context, id string) error {
	_ = p.fakeAPI.ContainerUnpause(ctx, id)
	p.mu.Lock()
	if p.resumedFirst {
		p.resumedFirst, p.paused = false, false
		p.mu.Unlock()
		return errors.New("container is not paused")
	}
	was := p.paused
	p.mu.Unlock()
	if !was {
		return errors.New("container is not paused")
	}
	time.Sleep(30 * time.Millisecond)
	p.mu.Lock()
	p.paused = false
	p.mu.Unlock()
	return nil
}

func (p *pausingDaemon) ContainerPause(ctx context.Context, id string) error {
	_ = p.fakeAPI.ContainerPause(ctx, id)
	p.mu.Lock()
	p.paused = true
	p.mu.Unlock()
	return nil
}

// Two copies into one paused desktop at once both succeed: the second joins the first's unpause instead of failing
// on Docker's "not paused", and the desktop is paused again once, after both (REVIEW-3 follow-up 3).
func TestTwoCopiesIntoAPausedDesktopBothSucceed(t *testing.T) {
	fake, _ := homeDesktop(true, true)
	daemon := &pausingDaemon{fakeAPI: fake, paused: true}
	m := newManagerWith(daemon)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = m.readHomeBytes(context.Background(), "box", homeDir+"/present", 1<<20)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("copy %d: %v", i, err)
		}
	}
	if n := len(fake.called("ContainerUnpause")); n != 1 {
		t.Errorf("want one unpause, got %d: %v", n, fake.calls)
	}
	if n := len(fake.called("ContainerPause")); n != 1 {
		t.Errorf("want one pause after both copies, got %d: %v", n, fake.calls)
	}
	if !daemon.paused {
		t.Error("the desktop must be paused again after the copies")
	}
}

// A desktop resumed by someone else between the copy's read and its unpause is copied into as a running desktop,
// and not paused afterwards: the copy did not pause it, so it is not the copy's to pause.
func TestACopyIntoADesktopResumedMeanwhileGoesOn(t *testing.T) {
	fake, _ := homeDesktop(true, true)
	daemon := &pausingDaemon{fakeAPI: fake, paused: true, resumedFirst: true}
	m := newManagerWith(daemon)
	if _, err := m.readHomeBytes(context.Background(), "box", homeDir+"/present", 1<<20); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.called("ContainerPause")); n != 0 {
		t.Errorf("a desktop the copy did not resume must not be paused by it: %v", fake.calls)
	}
}
