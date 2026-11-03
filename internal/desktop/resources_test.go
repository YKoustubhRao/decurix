package desktop

import (
	"context"
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
