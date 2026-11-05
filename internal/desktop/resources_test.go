package desktop

import (
	"context"
	"fmt"
	"testing"

	"github.com/docker/docker/api/types/container"
)

// TestCPUIsMeasuredAgainstThePreviousSample: a one-shot sample has no previous reading of its own, so the delta was
// everything since boot and a pegged desktop read under 1% (B-25).
func TestCPUIsMeasuredAgainstThePreviousSample(t *testing.T) {
	m := newManagerWith(&fakeAPI{})
	sample := func(total, system uint64) *container.StatsResponse {
		s := &container.StatsResponse{}
		s.CPUStats.CPUUsage.TotalUsage = total
		s.CPUStats.SystemUsage = system
		s.CPUStats.OnlineCPUs = 2
		m.withPreviousCPU("box", s)
		return s
	}
	first := sample(1_000_000, 100_000_000)
	if first.PreCPUStats.SystemUsage != first.CPUStats.SystemUsage {
		t.Fatal("a first sample must measure nothing, not everything since boot")
	}
	// Pegged on one of two CPUs for the interval: half of the system time went to it.
	second := sample(1_000_000+50_000_000, 200_000_000)
	if second.PreCPUStats.CPUUsage.TotalUsage != 1_000_000 {
		t.Fatalf("the previous sample was not used: %+v", second.PreCPUStats)
	}
}

// TestResourcesAreClampedToTheMachine: a request beyond the machine's CPUs or memory is cut to what it has.
func TestResourcesAreClampedToTheMachine(t *testing.T) {
	fake := &fakeAPI{}
	m := newManagerWith(fake)
	if err := m.UpdateResources(context.Background(), "box", 64, 1<<20); err != nil {
		t.Fatal(err)
	}
	if c := fake.called("ContainerUpdate desktop_box cpus=4 mem=8192"); len(c) != 1 {
		t.Fatalf("not clamped to 4 CPUs and 8 GB: %v", fake.calls)
	}
	if got := m.resourceOverrides["box"]; got.CPUs != 4 || got.MemoryMB != 8192 {
		t.Fatalf("the shown size must be what was given: %+v", got)
	}
}

// TestAListThatPanicsDoesNotHoldUpTheNext: the shared List call ends its flight even when it panics (B-32).
func TestAListThatPanicsDoesNotHoldUpTheNext(t *testing.T) {
	m := newManagerWith(&fakeAPI{}) // ContainerList is not implemented: the list panics
	for i := 0; i < 2; i++ {
		func() {
			defer func() { _ = recover() }()
			_, _ = m.List(context.Background())
		}()
	}
	m.listMu.Lock()
	defer m.listMu.Unlock()
	if m.listFlight != nil {
		t.Fatal("a panicked list left its flight set; every later List would wait on it")
	}
}

// TestAnOOMVerdictIsAskedOncePerExit: a stopped desktop costs one inspect, not one per List (B-33).
func TestAnOOMVerdictIsAskedOncePerExit(t *testing.T) {
	fake := &fakeAPI{inspect: func(string) (container.InspectResponse, error) {
		return container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{State: &container.State{OOMKilled: true}}}, nil
	}}
	m := newManagerWith(fake)
	for i := 0; i < 3; i++ {
		if !m.oomKilled(context.Background(), "c1", fmt.Sprintf("Exited (137) %d minutes ago", 5+i)) {
			t.Fatal("verdict lost")
		}
	}
	if n := len(fake.called("ContainerInspect")); n != 1 {
		t.Fatalf("inspected %d times, want once", n)
	}
}
