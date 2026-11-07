package desktop

import (
	"context"
	"net/netip"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
)

// C-25: only a practice desktop the control plane labelled and named for its run is known by its address on the
// practice network; the run's mock app, a container named like one but unlabelled, and an address on another network
// are not.
func TestPracticeDesktopAtKnowsOnlyTheRunsDesktop(t *testing.T) {
	on := func(net, ip string) *container.NetworkSettingsSummary {
		return &container.NetworkSettingsSummary{Networks: map[string]*network.EndpointSettings{net: {IPAddress: ip}}}
	}
	fake := &fakeAPI{containers: []container.Summary{
		{Names: []string{"/practice-r1"}, Labels: map[string]string{PracticeLabel: "true", "decurix-practice-run": "r1"},
			NetworkSettings: on(PracticeNetworkName, "172.30.0.5")},
		{Names: []string{"/practice-mock-r1"}, Labels: map[string]string{PracticeLabel: "true", "decurix-practice-run": "r1",
			"decurix-practice-mock": "true"}, NetworkSettings: on(PracticeNetworkName, "172.30.0.6")},
		{Names: []string{"/practice-r2"}, Labels: map[string]string{}, NetworkSettings: on(PracticeNetworkName, "172.30.0.7")},
		{Names: []string{"/practice-r3"}, Labels: map[string]string{PracticeLabel: "true", "decurix-practice-run": "r3"},
			NetworkSettings: on("bridge", "172.17.0.9")},
		{Names: []string{"/something-else"}, Labels: map[string]string{PracticeLabel: "true", "decurix-practice-run": "r4"},
			NetworkSettings: on(PracticeNetworkName, "172.30.0.8")},
	}}
	m := &Manager{client: fake}
	ctx := context.Background()
	if id, ok := m.PracticeDesktopAt(ctx, netip.MustParseAddr("172.30.0.5")); !ok || id != "practice-r1" {
		t.Fatalf("the run's desktop = %q %v", id, ok)
	}
	if id, ok := m.PracticeDesktopAt(ctx, netip.MustParseAddr("::ffff:172.30.0.5")); !ok || id != "practice-r1" {
		t.Fatalf("a mapped address = %q %v", id, ok)
	}
	for _, ip := range []string{"172.30.0.6", "172.30.0.7", "172.17.0.9", "172.30.0.8", "172.30.0.99"} {
		if id, ok := m.PracticeDesktopAt(ctx, netip.MustParseAddr(ip)); ok {
			t.Errorf("%s was taken for %s", ip, id)
		}
	}
	if _, ok := (&Manager{}).PracticeDesktopAt(ctx, netip.MustParseAddr("172.30.0.5")); ok {
		t.Error("a manager with no Docker answered")
	}
}
